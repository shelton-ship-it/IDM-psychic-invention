package torrentengine

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/torrent"
)

// ---- fixture: torrent multi-ficheiro local ----

type fixture struct {
	src  string
	raw  []byte
	meta *torrent.Meta
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	src := t.TempDir()
	write := func(rel string, n int, seed byte) {
		p := filepath.Join(src, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*13) ^ seed
		}
		os.WriteFile(p, b, 0o644)
	}
	write("a.bin", 10_000, 1)
	write("sub/b.bin", 7_000, 2)
	raw, err := torrent.BuildFixture(src, "pack", []string{"a.bin", "sub/b.bin"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	m, err := torrent.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{src: src, raw: raw, meta: m}
}

// ---- backend falso (sem rede) ----

type fakeBackend struct {
	mu        sync.Mutex
	metaRaw   []byte        // metainfo que a "rede" devolve (magnet)
	srcDir    string        // de onde "descarrega" o conteúdo
	meta      *torrent.Meta // para saber o layout
	corrupt   bool          // corrompe 1 byte após copiar
	gate      chan struct{} // se != nil, a cópia espera por gate (para testar pausa)
	metaBlock bool          // Meta() bloqueia até ctx terminar (timeout)

	adds      int
	downloads int
	stops     int
	selected  map[int]bool
	lastSrc   Source
}

func (b *fakeBackend) Add(ctx context.Context, src Source, destDir string) (Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adds++
	b.lastSrc = src
	return &fakeSession{b: b, destDir: destDir, done: make(chan struct{}), src: src}, nil
}
func (b *fakeBackend) Close() error { return nil }

type fakeSession struct {
	b       *fakeBackend
	destDir string
	src     Source
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	d, tot  int64
}

func (s *fakeSession) Meta(ctx context.Context) ([]byte, error) {
	if s.b.metaBlock {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if len(s.src.MetaInfo) > 0 {
		return s.src.MetaInfo, nil
	}
	return s.b.metaRaw, nil
}

func (s *fakeSession) Download(selected map[int]bool) error {
	s.b.mu.Lock()
	s.b.downloads++
	s.b.selected = selected
	s.b.mu.Unlock()
	m := s.b.meta
	go func() {
		if s.b.gate != nil {
			<-s.b.gate
		}
		for _, f := range m.Files {
			if selected != nil && !selected[f.Index] {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(s.b.srcDir, filepath.FromSlash(f.Path)))
			dst := filepath.Join(s.destDir, m.Name, filepath.FromSlash(f.Path))
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if s.b.corrupt && f.Index == 0 {
				data[5] ^= 0xFF
			}
			os.WriteFile(dst, data, 0o644)
			s.mu.Lock()
			s.d += int64(len(data))
			s.mu.Unlock()
		}
		s.once.Do(func() { close(s.done) })
	}()
	return nil
}
func (s *fakeSession) Progress() (int64, int64) { s.mu.Lock(); defer s.mu.Unlock(); return s.d, s.tot }
func (s *fakeSession) Done() <-chan struct{}    { return s.done }
func (s *fakeSession) Stop() error {
	s.b.mu.Lock()
	s.b.stops++
	s.b.mu.Unlock()
	return nil
}

// ---- sink + helpers ----

type sink struct {
	mu sync.Mutex
	ev []engine.Event
}

func (s *sink) Emit(e engine.Event) { s.mu.Lock(); s.ev = append(s.ev, e); s.mu.Unlock() }
func (s *sink) kinds() []engine.EventKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []engine.EventKind
	for _, e := range s.ev {
		k = append(k, e.Kind)
	}
	return k
}
func (s *sink) find(k engine.EventKind) *engine.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.ev {
		if s.ev[i].Kind == k {
			e := s.ev[i]
			return &e
		}
	}
	return nil
}
func (s *sink) lastErr() *domain.DownloadError {
	s.mu.Lock()
	defer s.mu.Unlock()
	var d *domain.DownloadError
	for _, e := range s.ev {
		if e.Kind == engine.EventError {
			d = e.Error
		}
	}
	return d
}

func magnetTask(id, dest, infoHash string) domain.DownloadTask {
	return domain.DownloadTask{ID: id, State: domain.StateRunning,
		Source: domain.DownloadSource{Kind: domain.SourceMagnet, URL: "magnet:?xt=urn:btih:" + infoHash, InfoHash: infoHash, DisplayName: "x"},
		Policy: domain.DownloadPolicy{DestinationDir: dest, RequireIntegrity: domain.IntegrityTorrentPieces}}
}

func run(t *testing.T, e *Engine, task domain.DownloadTask) (*sink, error) {
	t.Helper()
	s := &sink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := h.(*job)
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
	return s, j.err
}

func newEngine(t *testing.T, b *fakeBackend, mod func(*Options)) *Engine {
	t.Helper()
	o := Options{Backend: b, MetadataTimeout: 2 * time.Second, ProgressEvery: 10 * time.Millisecond}
	if mod != nil {
		mod(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// ---- testes ----

func TestMagnetHappyPathVerifiesPiecesBeforeCompleted(t *testing.T) {
	fx := newFixture(t)
	b := &fakeBackend{metaRaw: fx.raw, srcDir: fx.src, meta: fx.meta}
	e := newEngine(t, b, nil)
	dest := t.TempDir()
	s, err := run(t, e, magnetTask("t1", dest, fx.meta.InfoHash))
	if err != nil {
		t.Fatal(err)
	}
	ev := s.find(engine.EventCompleted)
	if ev == nil || ev.Payload["verified"] != "pieces" || ev.Payload["path"] != filepath.Join(dest, "pack") {
		t.Fatalf("Completed inesperado: %+v (kinds=%v)", ev, s.kinds())
	}
	if s.find(engine.EventFileDiscovered) == nil || s.find(engine.EventIntegrityReport) == nil {
		t.Errorf("faltam eventos: %v", s.kinds())
	}
	if b.stops == 0 {
		t.Error("sessão deve parar a rede antes de verificar")
	}
}

func TestInfoHashMismatchRejectsBeforeAnyDownload(t *testing.T) {
	fx := newFixture(t)
	// "rede" devolve OUTRO torrent para o hash pedido
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "z.bin"), make([]byte, 5000), 0o644)
	otherRaw, _ := torrent.BuildFixture(other, "z.bin", []string{"z.bin"}, 4096)
	b := &fakeBackend{metaRaw: otherRaw, srcDir: fx.src, meta: fx.meta}
	e := newEngine(t, b, nil)
	dest := t.TempDir()
	s, err := run(t, e, magnetTask("t2", dest, fx.meta.InfoHash))
	var de *domain.DownloadError
	if !errors.As(err, &de) || de.Class != domain.ErrIntegrityFailure {
		t.Fatalf("esperava ErrIntegrityFailure, obtido %v", err)
	}
	if b.downloads != 0 {
		t.Fatal("conteúdo foi descarregado apesar de metadados falsos")
	}
	if es, _ := os.ReadDir(dest); len(es) != 0 {
		t.Errorf("nada deve ser gravado: %v", es)
	}
	if s.lastErr() == nil || s.find(engine.EventCompleted) != nil {
		t.Error("deve haver erro e nenhum Completed")
	}
}

func TestHostileMetadataFromMagnetIsRejected(t *testing.T) {
	// metainfo com caminho "../.." vindo da rede
	info := map[string]any{"name": "p", "piece length": int64(16384),
		"pieces": make([]byte, 20),
		"files":  []any{map[string]any{"length": int64(10), "path": []any{"..", "..", "evil"}}}}
	raw, _ := torrent.Encode(map[string]any{"info": info})
	b := &fakeBackend{metaRaw: raw}
	e := newEngine(t, b, nil)
	_, err := run(t, e, magnetTask("t3", t.TempDir(), strings.Repeat("a", 40)))
	var de *domain.DownloadError
	if !errors.As(err, &de) || de.Class != domain.ErrPolicyViolation {
		t.Fatalf("esperava ErrPolicyViolation, obtido %v", err)
	}
	if b.downloads != 0 {
		t.Fatal("não pode descarregar com metadados inseguros")
	}
}

func TestCorruptedDataFailsIntegrityNeverCompleted(t *testing.T) {
	fx := newFixture(t)
	b := &fakeBackend{metaRaw: fx.raw, srcDir: fx.src, meta: fx.meta, corrupt: true}
	e := newEngine(t, b, nil)
	s, err := run(t, e, magnetTask("t4", t.TempDir(), fx.meta.InfoHash))
	var de *domain.DownloadError
	if !errors.As(err, &de) || de.Class != domain.ErrIntegrityFailure || !de.Retryable {
		t.Fatalf("esperava ErrIntegrityFailure retomável, obtido %v", err)
	}
	if s.find(engine.EventCompleted) != nil {
		t.Fatal("Completed emitido com dados corrompidos")
	}
	if r := s.find(engine.EventIntegrityReport); r == nil || r.Payload["result"] != "mismatch" {
		t.Errorf("relatório de integridade em falta: %+v", r)
	}
}

func TestTorrentFileSourceAndHostileTorrentRejectedBeforeBackend(t *testing.T) {
	fx := newFixture(t)
	tf := filepath.Join(t.TempDir(), "x.torrent")
	os.WriteFile(tf, fx.raw, 0o644)
	b := &fakeBackend{srcDir: fx.src, meta: fx.meta}
	e := newEngine(t, b, nil)
	task := domain.DownloadTask{ID: "t5", Policy: domain.DownloadPolicy{DestinationDir: t.TempDir()},
		Source: domain.DownloadSource{Kind: domain.SourceTorrent, URL: "file://" + tf}}
	s, err := run(t, e, task)
	if err != nil || s.find(engine.EventCompleted) == nil {
		t.Fatalf(".torrent local: %v %v", err, s.kinds())
	}
	if len(b.lastSrc.MetaInfo) == 0 {
		t.Error("o Backend deveria receber o MetaInfo do .torrent")
	}
	// .torrent hostil: nunca chega ao Backend
	hostile, _ := torrent.Encode(map[string]any{"info": map[string]any{"name": "../x", "piece length": int64(16384),
		"pieces": make([]byte, 20), "length": int64(5)}})
	hf := filepath.Join(t.TempDir(), "h.torrent")
	os.WriteFile(hf, hostile, 0o644)
	b2 := &fakeBackend{}
	e2 := newEngine(t, b2, nil)
	task.ID, task.Source.URL = "t5b", "file://"+hf
	if _, err := run(t, e2, task); err == nil {
		t.Fatal(".torrent com nome inseguro deveria falhar")
	}
	if b2.adds != 0 {
		t.Fatal("torrent inseguro não pode chegar ao Backend")
	}
}

func TestFileSelectionAndInvalidSelection(t *testing.T) {
	fx := newFixture(t)
	b := &fakeBackend{metaRaw: fx.raw, srcDir: fx.src, meta: fx.meta}
	e := newEngine(t, b, nil)
	task := magnetTask("t6", t.TempDir(), fx.meta.InfoHash)
	task.Files = []domain.FileMetadata{{Index: 0, Selected: true}}
	s, err := run(t, e, task)
	if err != nil || s.find(engine.EventCompleted) == nil {
		t.Fatalf("seleção parcial: %v %v", err, s.kinds())
	}
	if !b.selected[0] || b.selected[1] {
		t.Errorf("Backend recebeu seleção errada: %v", b.selected)
	}
	// índice inexistente é recusado, não ignorado
	b2 := &fakeBackend{metaRaw: fx.raw, srcDir: fx.src, meta: fx.meta}
	e2 := newEngine(t, b2, nil)
	bad := magnetTask("t6b", t.TempDir(), fx.meta.InfoHash)
	bad.Files = []domain.FileMetadata{{Index: 9, Selected: true}}
	if _, err := run(t, e2, bad); err == nil || b2.downloads != 0 {
		t.Fatalf("seleção inválida deveria falhar antes de descarregar (err=%v)", err)
	}
}

func TestMaxSizePolicyBlocksBeforeDownload(t *testing.T) {
	fx := newFixture(t)
	b := &fakeBackend{metaRaw: fx.raw, srcDir: fx.src, meta: fx.meta}
	e := newEngine(t, b, nil)
	task := magnetTask("t7", t.TempDir(), fx.meta.InfoHash)
	task.Policy.MaxSizeBytes = 1000
	_, err := run(t, e, task)
	var de *domain.DownloadError
	if !errors.As(err, &de) || de.Class != domain.ErrPolicyViolation || b.downloads != 0 {
		t.Fatalf("esperava ErrPolicyViolation sem download: %v downloads=%d", err, b.downloads)
	}
}

func TestMetadataTimeoutIsRetryableConnectionLost(t *testing.T) {
	b := &fakeBackend{metaBlock: true}
	e := newEngine(t, b, func(o *Options) { o.MetadataTimeout = 50 * time.Millisecond })
	_, err := run(t, e, magnetTask("t8", t.TempDir(), strings.Repeat("b", 40)))
	var de *domain.DownloadError
	if !errors.As(err, &de) || de.Class != domain.ErrConnectionLost || !de.Retryable {
		t.Fatalf("esperava ErrConnectionLost retomável, obtido %v", err)
	}
}

func TestPauseStopsSessionThenRestartCompletes(t *testing.T) {
	fx := newFixture(t)
	gate := make(chan struct{})
	b := &fakeBackend{metaRaw: fx.raw, srcDir: fx.src, meta: fx.meta, gate: gate}
	e := newEngine(t, b, nil)
	dest := t.TempDir()
	task := magnetTask("t9", dest, fx.meta.InfoHash)
	s := &sink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s})
	if err != nil {
		t.Fatal(err)
	}
	// esperar o Download começar (bloqueado no gate)
	for i := 0; i < 500 && func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.downloads == 0 }(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Start imediato após pausa deve esperar a job anterior (sem "já ativa")
	close(gate) // o novo download não bloqueia
	s2 := &sink{}
	h2, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s2})
	if err != nil {
		t.Fatalf("Start após pausa: %v", err)
	}
	<-h2.(*job).done
	if h2.(*job).err != nil || s2.find(engine.EventCompleted) == nil {
		t.Fatalf("retoma não completou: %v %v", h2.(*job).err, s2.kinds())
	}
	if s.lastErr() != nil {
		t.Errorf("pausa não pode emitir erro: %+v", s.lastErr())
	}
	if b.stops < 1 {
		t.Error("pausa deve parar a sessão")
	}
}

func TestInfoHashNormalizationBase32(t *testing.T) {
	raw := make([]byte, 20)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	b32 := base32.StdEncoding.EncodeToString(raw)
	got, err := normalizeInfoHash(strings.ToLower(b32))
	if err != nil || got != hex.EncodeToString(raw) {
		t.Fatalf("base32 → hex falhou: %q %v", got, err)
	}
	if _, err := normalizeInfoHash("curto"); err == nil {
		t.Error("tamanho inválido deve falhar")
	}
	if _, err := normalizeInfoHash(strings.Repeat("z", 40)); err == nil {
		t.Error("hex inválido deve falhar")
	}
}

func TestNewRequiresBackendAndCanHandle(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("Backend obrigatório")
	}
	e := newEngine(t, &fakeBackend{}, nil)
	if !e.CanHandle(domain.DownloadSource{Kind: domain.SourceMagnet}) || e.CanHandle(domain.DownloadSource{Kind: domain.SourceHTTP}) {
		t.Error("CanHandle incorreto")
	}
}

type selfVerifyingSession struct{ *fakeSession }

func (selfVerifyingSession) VerifiesPieces() bool { return true }

type selfVerifyingBackend struct{ *fakeBackend }

func (b selfVerifyingBackend) Add(ctx context.Context, src Source, dest string) (Session, error) {
	s, err := b.fakeBackend.Add(ctx, src, dest)
	if err != nil {
		return nil, err
	}
	return selfVerifyingSession{s.(*fakeSession)}, nil
}

// Seleção sem nenhuma peça inteira dentro dos ficheiros escolhidos: o disco não
// prova nada. Só um backend que declare verificar cada peça pode concluir.
func TestSelectionWithoutWholePiecesNeedsBackendProof(t *testing.T) {
	src := t.TempDir()
	for i, n := range []string{"f0.bin", "f1.bin", "f2.bin"} {
		d := make([]byte, 3000)
		for j := range d {
			d[j] = byte(j + i*50)
		}
		os.WriteFile(filepath.Join(src, n), d, 0o644)
	}
	raw, err := torrent.BuildFixture(src, "trio", []string{"f0.bin", "f1.bin", "f2.bin"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := torrent.Parse(raw)
	mk := func(sv bool) (*sink, error) {
		fb := &fakeBackend{metaRaw: raw, srcDir: src, meta: meta}
		var be Backend = fb
		if sv {
			be = selfVerifyingBackend{fb}
		}
		e := newEngine(t, nil, nil)
		e.opts.Backend = be
		task := magnetTask("sel", t.TempDir(), meta.InfoHash)
		task.Files = []domain.FileMetadata{{Index: 1, Selected: true}} // f1: nenhuma peça inteira
		return run(t, e, task)
	}
	s, err := mk(false)
	var de *domain.DownloadError
	if !errors.As(err, &de) || de.Class != domain.ErrIntegrityFailure || s.find(engine.EventCompleted) != nil {
		t.Fatalf("sem prova do backend deve falhar: %v", err)
	}
	s, err = mk(true)
	if err != nil || s.find(engine.EventCompleted) == nil {
		t.Fatalf("com prova do backend deve concluir: %v (%v)", err, s.kinds())
	}
	if r := s.find(engine.EventIntegrityReport); r == nil || r.Payload["proof"] != "backend" {
		t.Errorf("relatório deve indicar a origem da prova: %+v", r)
	}
}
