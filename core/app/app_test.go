package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"streamvault/core/config"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/httpengine"
	"streamvault/core/lan"
	"streamvault/core/organizer"
	"streamvault/core/storage"
	"streamvault/core/torrent"
	"streamvault/core/torrentengine"
)

func testCfg(dir string) config.Config {
	c := config.Default()
	c.Storage.DefaultDir = dir
	c.Security.AllowPrivateNetworks = true // servidores httptest são loopback
	return c
}

func waitState(t *testing.T, st Persistence, id string, want domain.State) *domain.DownloadTask {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if tk, err := st.Get(id); err == nil && tk.State == want {
			return tk
		}
		time.Sleep(10 * time.Millisecond)
	}
	tk, _ := st.Get(id)
	t.Fatalf("timeout à espera de %s; estado atual: %+v", want, tk)
	return nil
}

func payloadOf(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 241)
	}
	return p
}

// Conversão de retoma: ida e volta sem perder identidade remota.
func TestResumeConversionRoundTrip(t *testing.T) {
	in := httpengine.ResumeState{TaskID: "t1", TempPath: "/tmp/a.svpart", BytesDone: 4096, Total: 99999,
		ETag: `"x"`, LastModified: "Mon, 01 Jan 2026 00:00:00 GMT"}
	out, ok := fromStorageResume(ptr(toStorageResume(in)))
	if !ok || out.BytesDone != 4096 || out.Total != 99999 || out.ETag != `"x"` || out.TempPath != in.TempPath || out.LastModified != in.LastModified {
		t.Fatalf("ida e volta divergiu: %+v ok=%v", out, ok)
	}
	if _, ok := fromStorageResume(&storage.ResumeState{TaskID: "t", Engine: "torrent",
		TempPath: "x", Offsets: map[string]int64{"0": 5}}); ok {
		t.Error("estado de outro motor deve ser recusado")
	}
	if _, ok := fromStorageResume(&storage.ResumeState{TaskID: "t", Engine: "http"}); ok {
		t.Error("estado sem offset deve ser recusado")
	}
}

func ptr[T any](v T) *T { return &v }

// Ponta a ponta: Add → motor HTTP real → Verifying → Completed + índice de hash.
func TestEndToEndHTTPDownload(t *testing.T) {
	data := payloadOf(300 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", time.Unix(0, 0), strings.NewReader(string(data)))
	}))
	defer srv.Close()
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.Add(srv.URL+"/f.bin", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	got, err := os.ReadFile(filepath.Join(dir, "f.bin"))
	if err != nil || len(got) != len(data) {
		t.Fatalf("ficheiro final: %v len=%d", err, len(got))
	}
	sum := sha256.Sum256(data)
	// o índice é escrito logo após a transição; esperar brevemente
	var e storage.HashEntry
	var ok bool
	for i := 0; i < 200 && !ok; i++ {
		st.mu.Lock()
		e, ok = st.hashes[hex.EncodeToString(sum[:])]
		st.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || e.SourceTask != tk.ID {
		t.Fatalf("índice de hashes não registou o ficheiro concluído: %+v ok=%v", e, ok)
	}
}

// Pausar e retomar de ponta a ponta: o servidor recebe Range e o conteúdo final é idêntico.
func TestEndToEndPauseResume(t *testing.T) {
	data := payloadOf(400 * 1024)
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" {
			mu.Lock()
			ranges = append(ranges, rg)
			mu.Unlock()
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "big.bin", time.Unix(0, 0), strings.NewReader(string(data)))
	}))
	defer srv.Close()
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.Add(srv.URL+"/big.bin", domain.DownloadPolicy{SpeedLimitBps: 100 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "big.bin.svpart")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Stat(part); err == nil && fi.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download não avançou")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.Orch.Pause(tk.ID); err != nil {
		t.Fatalf("pausa: %v", err)
	}
	waitState(t, st, tk.ID, domain.StatePaused)
	if err := c.Orch.Resume(tk.ID); err != nil {
		t.Fatalf("retoma: %v", err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)

	got, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := sha256.Sum256(got), sha256.Sum256(data)
	if a != b {
		t.Fatal("conteúdo corrompido após pausa/retoma")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) == 0 {
		t.Error("a retoma não enviou Range ao servidor")
	}
}

// Modo estrito (allow_private_networks=false): loopback é recusado antes de qualquer pedido.
func TestAddBlocksPrivateNetworkInStrictMode(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.Security.AllowPrivateNetworks = false
	c, err := New(cfg, NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Add("http://127.0.0.1:8080/x.bin", domain.DownloadPolicy{})
	de, ok := err.(*domain.DownloadError)
	if !ok || de.Class != domain.ErrPolicyViolation {
		t.Fatalf("esperava ErrPolicyViolation, obtido %#v", err)
	}
	if l, _ := c.Store.List(); len(l) != 0 {
		t.Error("origem bloqueada não pode entrar na fila")
	}
}

// Magnet sem motor BitTorrent (F3 pendente): falha explicada, não silêncio.
func TestMagnetWithoutTorrentEngineFailsHonestly(t *testing.T) {
	st := NewMemStore()
	c, err := New(testCfg(t.TempDir()), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tk, err := c.Add("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(tk.ID)
	if got.State != domain.StateFailed || got.Error == nil || got.Error.Class != domain.ErrUnsupportedSource {
		t.Fatalf("esperava failed/unsupported_source, obtido %s %+v", got.State, got.Error)
	}
}

func waitHash(t *testing.T, st *MemStore, sum string) storage.HashEntry {
	t.Helper()
	for i := 0; i < 300; i++ {
		st.mu.Lock()
		e, ok := st.hashes[sum]
		st.mu.Unlock()
		if ok {
			return e
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("índice de hashes não foi atualizado")
	return storage.HashEntry{}
}

// F4+F9: o 2.º ficheiro idêntico é detetado como duplicado do 1.º (já
// organizado), e nada é apagado.
func TestEndToEndOrganizeAndDuplicateDetection(t *testing.T) {
	data := payloadOf(64 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.mp4", time.Unix(0, 0), strings.NewReader(string(data)))
	}))
	defer srv.Close()
	dl, lib := t.TempDir(), t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dl), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	a, _ := c.Add(srv.URL+"/a.mp4", domain.DownloadPolicy{})
	waitState(t, st, a.ID, domain.StateCompleted)
	waitHash(t, st, hexSum)
	resA, err := c.Organize(a.ID, organizer.Options{BaseDir: lib})
	if err != nil || resA.Action != organizer.ActionMoved || resA.Category != organizer.CatVideo {
		t.Fatalf("organizar A: %+v %v", resA, err)
	}
	if _, err := os.Stat(filepath.Join(lib, organizer.CatVideo, "a.mp4")); err != nil {
		t.Fatalf("A não está na pasta da categoria: %v", err)
	}
	if e := waitHash(t, st, hexSum); e.Path != resA.NewPath {
		t.Fatalf("índice devia apontar ao novo local de A: %q != %q", e.Path, resA.NewPath)
	}

	b, _ := c.Add(srv.URL+"/b.mp4", domain.DownloadPolicy{})
	waitState(t, st, b.ID, domain.StateCompleted)
	resB, err := c.Organize(b.ID, organizer.Options{BaseDir: lib})
	if err != nil || resB.Action != organizer.ActionDuplicate || len(resB.DuplicateOf) != 1 || resB.DuplicateOf[0].Path != resA.NewPath {
		t.Fatalf("B deveria ser duplicado de A: %+v %v", resB, err)
	}
	if _, err := os.Stat(filepath.Join(dl, "b.mp4")); err != nil {
		t.Fatal("duplicado não pode ser movido/apagado sem decisão do utilizador")
	}
	// utilizador decide manter ambos
	resB2, err := c.Organize(b.ID, organizer.Options{BaseDir: lib, MoveDuplicates: true})
	if err != nil || resB2.Action != organizer.ActionMoved {
		t.Fatalf("manter ambos: %+v %v", resB2, err)
	}
	if _, err := c.Organize("inexistente", organizer.Options{BaseDir: lib}); err == nil {
		t.Error("tarefa inexistente deve falhar")
	}
}

type demoAdapter struct{ base string }

func (demoAdapter) Name() string           { return "demo" }
func (demoAdapter) Version() string        { return "1.0.0" }
func (demoAdapter) Schemes() []string      { return []string{"demo"} }
func (demoAdapter) Restrictions() []string { return []string{"Só conteúdo público, sem DRM."} }
func (demoAdapter) CanRecognize(raw string) bool {
	return strings.HasPrefix(raw, "demo://")
}
func (d demoAdapter) Parse(raw string) (domain.DownloadSource, error) {
	return domain.DownloadSource{Kind: domain.SourceHTTP, URL: d.base + "/" + strings.TrimPrefix(raw, "demo://")}, nil
}

// F11: adaptador só atua depois de aprovado; depois resolve e descarrega.
func TestPluginRequiresApprovalThenDownloads(t *testing.T) {
	data := payloadOf(8 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "v.bin", time.Unix(0, 0), strings.NewReader(string(data)))
	}))
	defer srv.Close()
	st := NewMemStore()
	c, err := New(testCfg(t.TempDir()), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Plugins.Register(demoAdapter{base: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add("demo://v.bin", domain.DownloadPolicy{}); err == nil {
		t.Fatal("adaptador não aprovado não pode resolver links")
	}
	if l, _ := st.List(); len(l) != 0 {
		t.Fatal("link bloqueado entrou na fila")
	}
	c.Plugins.Approve("demo")
	tk, err := c.Add("demo://v.bin", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	if tk.Source.Metadata["adapter"] != "demo@1.0.0" {
		t.Errorf("origem sem rasto do adaptador: %+v", tk.Source.Metadata)
	}
}

// ---- F3 ponta a ponta com backend BitTorrent falso (sem rede) ----

type btBackend struct {
	raw     []byte
	src     string
	meta    *torrent.Meta
	corrupt bool
}

func (b *btBackend) Add(ctx context.Context, src torrentengine.Source, dest string) (torrentengine.Session, error) {
	return &btSession{b: b, dest: dest, done: make(chan struct{})}, nil
}
func (b *btBackend) Close() error { return nil }

type btSession struct {
	b    *btBackend
	dest string
	done chan struct{}
}

func (s *btSession) Meta(ctx context.Context) ([]byte, error) { return s.b.raw, nil }
func (s *btSession) Download(sel map[int]bool) error {
	go func() {
		for _, f := range s.b.meta.Files {
			data, _ := os.ReadFile(filepath.Join(s.b.src, filepath.FromSlash(f.Path)))
			dst := filepath.Join(s.dest, s.b.meta.Name, filepath.FromSlash(f.Path))
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if s.b.corrupt && f.Index == 0 {
				data[3] ^= 0xFF
			}
			os.WriteFile(dst, data, 0o644)
		}
		close(s.done)
	}()
	return nil
}
func (s *btSession) Progress() (int64, int64) { return 0, 0 }
func (s *btSession) Done() <-chan struct{}    { return s.done }
func (s *btSession) Stop() error              { return nil }

func btFixture(t *testing.T) (string, []byte, *torrent.Meta) {
	t.Helper()
	src := t.TempDir()
	data := payloadOf(9000)
	os.WriteFile(filepath.Join(src, "movie.bin"), data, 0o644)
	os.WriteFile(filepath.Join(src, "sub.srt"), payloadOf(3000), 0o644)
	raw, err := torrent.BuildFixture(src, "release", []string{"movie.bin", "sub.srt"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	m, err := torrent.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return src, raw, m
}

// Magnet → motor torrent → verificação por peças → Verifying → Completed.
func TestEndToEndMagnetCompletesOnlyWithPieceVerification(t *testing.T) {
	src, raw, m := btFixture(t)
	dl := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dl), st, WithTorrentBackend(&btBackend{raw: raw, src: src, meta: m}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tk, err := c.Add("magnet:?xt=urn:btih:"+m.InfoHash+"&dn=release", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitState(t, st, tk.ID, domain.StateCompleted)
	if got.Policy.RequireIntegrity != domain.IntegrityTorrentPieces {
		t.Errorf("política deveria exigir verificação por peças: %v", got.Policy.RequireIntegrity)
	}
	if _, err := os.Stat(filepath.Join(dl, "release", "sub.srt")); err != nil {
		t.Errorf("conteúdo em falta: %v", err)
	}
}

// Dados corrompidos → Failed com integrity_failure; nunca Completed.
func TestEndToEndMagnetCorruptedDataFails(t *testing.T) {
	src, raw, m := btFixture(t)
	st := NewMemStore()
	c, err := New(testCfg(t.TempDir()), st, WithTorrentBackend(&btBackend{raw: raw, src: src, meta: m, corrupt: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tk, _ := c.Add("magnet:?xt=urn:btih:"+m.InfoHash, domain.DownloadPolicy{})
	got := waitState(t, st, tk.ID, domain.StateFailed)
	if got.Error == nil || got.Error.Class != domain.ErrIntegrityFailure {
		t.Fatalf("esperava integrity_failure, obtido %+v", got.Error)
	}
}

// F10: link enviado por outro dispositivo pareado entra pelo Core.Add e descarrega;
// sem o direito LANTransfer da licença, nada funciona.
func TestLANLinkHandoffRequiresLicenseAndGoesThroughAdd(t *testing.T) {
	data := payloadOf(20 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "h.bin", time.Unix(0, 0), strings.NewReader(string(data)))
	}))
	defer srv.Close()
	st := NewMemStore()
	c, err := New(testCfg(t.TempDir()), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.StartLAN("PC", t.TempDir(), "127.0.0.1:0", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.LANPairing(); err == nil {
		t.Fatal("sem licença LANTransfer o pareamento deve ser recusado")
	}
	c.SetEntitlements(domain.Entitlements{LANTransfer: true})
	pin, _, err := c.LANPairing()
	if err != nil {
		t.Fatal(err)
	}
	s, err := lan.Connect(context.Background(), c.LAN.Addr, pin, lan.SendOptions{Name: "telemóvel"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SendLink(srv.URL + "/h.bin"); err != nil {
		t.Fatalf("SendLink: %v", err)
	}
	l, _ := st.List()
	if len(l) != 1 {
		t.Fatalf("o link recebido devia ter entrado na fila: %d tarefas", len(l))
	}
	waitState(t, st, l[0].ID, domain.StateCompleted)
	// link inseguro: rede privada bloqueada pela política do núcleo (config sem AllowPrivate)
	c.Cfg.Security.AllowPrivateNetworks = false
	var rej *lan.RejectedError
	if err := s.SendLink("http://127.0.0.1:9/x"); !errors.As(err, &rej) {
		t.Fatalf("link bloqueado pela política deve ser recusado: %v", err)
	}
	// ficheiros sem confirmação do utilizador são recusados
	src := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(src, []byte("x"), 0o644)
	if err := s.SendFile(src); !errors.As(err, &rej) {
		t.Fatalf("ficheiro sem confirmação deve ser recusado: %v", err)
	}
}

// Por defeito (config.Default) um URL local/NAS colado pelo utilizador descarrega.
func TestDefaultConfigDownloadsFromLocalServer(t *testing.T) {
	data := payloadOf(30 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "n.bin", time.Unix(0, 0), strings.NewReader(string(data)))
	}))
	defer srv.Close()
	cfg := config.Default() // sem nenhuma alteração de segurança
	cfg.Storage.DefaultDir = t.TempDir()
	st := NewMemStore()
	c, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tk, err := c.Add(srv.URL+"/n.bin", domain.DownloadPolicy{})
	if err != nil {
		t.Fatalf("o defeito não pode bloquear servidores locais: %v", err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
}

// A retoma por segmento sobrevive à ida e volta pelo formato de armazenamento;
// estados incompletos ou incoerentes recomeçam limpos (nunca adivinhar).
func TestSegmentedResumeConversionRoundTripAndRejection(t *testing.T) {
	in := httpengine.ResumeState{TaskID: "t", TempPath: "/tmp/a.svpart", Total: 10_000_000, ETag: `"e"`,
		LastModified: "lm", SegSize: 4_000_000, SegDone: []int64{4_000_000, 1_500_000, 0}}
	st := toStorageResume(in)
	if st.Offsets["seg_size"] != 4_000_000 || st.Offsets["s0"] != 4_000_000 || st.Offsets["s1"] != 1_500_000 || st.Offsets["s2"] != 0 {
		t.Fatalf("formato persistido inesperado: %+v", st.Offsets)
	}
	out, ok := fromStorageResume(&st)
	if !ok || out.SegSize != 4_000_000 || len(out.SegDone) != 3 || out.BytesDone != 5_500_000 || out.Total != 10_000_000 || out.ETag != `"e"` {
		t.Fatalf("ida e volta divergiu: %+v ok=%v", out, ok)
	}
	// segmento em falta nas chaves => recusado
	delete(st.Offsets, "s1")
	if _, ok := fromStorageResume(&st); ok {
		t.Error("estado com segmento em falta deve ser recusado")
	}
	// nada feito (soma 0), tamanho desconhecido ou sem ficheiro temporário => recusado
	zero := toStorageResume(httpengine.ResumeState{TaskID: "t", TempPath: "/tmp/x", Total: 100, SegSize: 50, SegDone: []int64{0, 0}})
	if _, ok := fromStorageResume(&zero); ok {
		t.Error("sem progresso não há retoma")
	}
	noTotal := toStorageResume(in)
	noTotal.RemoteSize = 0
	if _, ok := fromStorageResume(&noTotal); ok {
		t.Error("sem tamanho remoto não há retoma segura")
	}
	// o estado "limpo" que o motor grava ao desistir de uma retoma
	cleared := toStorageResume(httpengine.ResumeState{TaskID: "t"})
	if _, ok := fromStorageResume(&cleared); ok {
		t.Error("estado limpo não pode ser tratado como retoma")
	}
}

// Produto completo: Core.Add → motor com várias ligações → pausa → o que fica
// gravado → retoma → ficheiro idêntico. O servidor limita cada ligação.
func TestCoreSegmentedDownloadPauseResumeThroughStorageFormat(t *testing.T) {
	data := payloadOf(8 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"seg-e2e"`)
		sw := &throttledWriter{ResponseWriter: w, bps: 2 << 20}
		http.ServeContent(sw, r, "big.bin", time.Unix(0, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := testCfg(dir)
	cfg.Network.ConnectionsPerDownload = 4
	st := NewMemStore()
	c, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var maxDone int64
	c.Events.Subscribe(func(e engine.Event) {
		if e.Kind == engine.EventProgress {
			atomic.StoreInt64(&maxDone, e.Progress.BytesDone)
		}
	})
	tk, err := c.Add(srv.URL+"/big.bin", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; atomic.LoadInt64(&maxDone) < 2<<20; i++ {
		if i > 1000 {
			t.Fatal("sem progresso")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Orch.Pause(tk.ID); err != nil {
		t.Fatalf("pausa: %v", err)
	}
	waitState(t, st, tk.ID, domain.StatePaused)

	// o que foi realmente persistido: um offset por segmento
	var rs storage.ResumeState
	for i := 0; i < 200; i++ {
		got, _ := st.LoadResume(tk.ID)
		if got != nil && got.Offsets["seg_size"] > 0 {
			rs = *got
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rs.Offsets["seg_size"] <= 0 || rs.RemoteSize != int64(len(data)) {
		t.Fatalf("a pausa devia gravar a retoma por segmento: %+v", rs)
	}
	var sum int64
	nseg := 0
	for k, v := range rs.Offsets {
		if strings.HasPrefix(k, "s") && k != "seg_size" {
			sum += v
			nseg++
		}
	}
	if nseg < 4 || sum < 2<<20 {
		t.Fatalf("esperava ≥4 segmentos e ≥2 MiB feitos, obtive %d segmentos e %d bytes", nseg, sum)
	}

	if err := c.Orch.Resume(tk.ID); err != nil {
		t.Fatalf("retoma: %v", err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	got, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("conteúdo diverge após pausa/retoma (err=%v, %d/%d bytes)", err, len(got), len(data))
	}
}

type throttledWriter struct {
	http.ResponseWriter
	bps int
}

func (w *throttledWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		c := 32 << 10
		if c > len(p) {
			c = len(p)
		}
		n, err := w.ResponseWriter.Write(p[:c])
		total += n
		if err != nil {
			return total, err
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(time.Duration(float64(n) / float64(w.bps) * float64(time.Second)))
		p = p[c:]
	}
	return total, nil
}
