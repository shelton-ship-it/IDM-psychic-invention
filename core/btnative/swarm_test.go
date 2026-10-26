package btnative

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/torrent"
	"streamvault/core/torrentengine"
)

// ---- fixture: torrent multi-ficheiro, peças que cruzam ficheiros ----

type fx struct {
	src    string // pasta com a.bin, sub/b.bin, c.bin
	raw    []byte
	meta   *torrent.Meta
	stream []byte // conteúdo concatenado (para peers de referência)
	ih     [20]byte
}

func content(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131+i/7) ^ seed
	}
	return b
}

func newFx(t *testing.T) fx {
	t.Helper()
	src := t.TempDir()
	files := []struct {
		rel  string
		size int
		seed byte
	}{{"a.bin", 100_000, 1}, {"sub/b.bin", 60_000, 2}, {"c.bin", 30_000, 3}}
	var rel []string
	var stream []byte
	for _, f := range files {
		p := filepath.Join(src, filepath.FromSlash(f.rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		d := content(f.size, f.seed)
		os.WriteFile(p, d, 0o644)
		rel = append(rel, f.rel)
		stream = append(stream, d...)
	}
	raw, err := torrent.BuildFixture(src, "pack", rel, 32768)
	if err != nil {
		t.Fatal(err)
	}
	m, err := torrent.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var ih [20]byte
	b, _ := hex.DecodeString(m.InfoHash)
	copy(ih[:], b)
	return fx{src: src, raw: raw, meta: m, stream: stream, ih: ih}
}

// layout de seeding: <dir>/pack/<path>
func (f fx) seedDir(t *testing.T) string {
	dir := t.TempDir()
	for _, file := range f.meta.Files {
		d, _ := os.ReadFile(filepath.Join(f.src, filepath.FromSlash(file.Path)))
		p := filepath.Join(dir, "pack", filepath.FromSlash(file.Path))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, d, 0o644)
	}
	return dir
}

func testBackend(t *testing.T, mod func(*Options)) *Backend {
	t.Helper()
	o := Options{ListenAddr: "127.0.0.1:0", AllowPrivatePeers: true, DialTimeout: 2 * time.Second}
	if mod != nil {
		mod(&o)
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// startSeeder: backend real com os dados completos, a servir.
func startSeeder(t *testing.T, f fx) *Backend {
	t.Helper()
	b := testBackend(t, nil)
	s, err := b.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, f.seedDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Meta(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Download(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("seeder não reconheceu os dados completos")
	}
	return b
}

// ---- tracker de teste ----

type tracker struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []string
	lefts  []string
}

func newTracker(t *testing.T, ih [20]byte, peers ...string) *tracker {
	tr := &tracker{}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("info_hash") != string(ih[:]) {
			b, _ := torrent.Encode(map[string]any{"failure reason": "torrent desconhecido"})
			w.Write(b)
			return
		}
		tr.mu.Lock()
		tr.events = append(tr.events, q.Get("event"))
		tr.lefts = append(tr.lefts, q.Get("left"))
		tr.mu.Unlock()
		var compact []byte
		for _, p := range peers {
			a, _ := net.ResolveTCPAddr("tcp", p)
			compact = append(compact, a.IP.To4()...)
			compact = append(compact, byte(a.Port>>8), byte(a.Port))
		}
		b, _ := torrent.Encode(map[string]any{"interval": int64(60), "peers": string(compact)})
		w.Write(b)
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *tracker) has(ev string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, e := range tr.events {
		if e == ev {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func magnetFor(f fx, trackerURL string) torrentengine.Source {
	m := "magnet:?xt=urn:btih:" + f.meta.InfoHash + "&dn=pack"
	if trackerURL != "" {
		m += "&tr=" + url.QueryEscape(trackerURL+"/announce")
	}
	return torrentengine.Source{Magnet: m, InfoHash: f.meta.InfoHash}
}

func sameContent(t *testing.T, f fx, dest string, only map[int]bool) {
	t.Helper()
	for _, file := range f.meta.Files {
		p := filepath.Join(dest, "pack", filepath.FromSlash(file.Path))
		got, err := os.ReadFile(p)
		if only != nil && !only[file.Index] {
			if err == nil {
				t.Errorf("ficheiro não selecionado foi criado: %s", file.Path)
			}
			continue
		}
		want, _ := os.ReadFile(filepath.Join(f.src, filepath.FromSlash(file.Path)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("conteúdo de %s diverge (err=%v, %d/%d bytes)", file.Path, err, len(got), len(want))
		}
	}
}

// ---- cenários ----

func TestMagnetMetadataExchangeThenFullDownload(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	tr := newTracker(t, f.ih, seeder.Addr().String())
	leech := testBackend(t, nil)
	dest := t.TempDir()
	s, err := leech.Add(context.Background(), magnetFor(f, tr.srv.URL), dest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := s.Meta(ctx) // só com o info-hash: metadados vêm do seeder (BEP 9)
	if err != nil {
		t.Fatalf("metadados: %v", err)
	}
	m, err := torrent.Parse(raw)
	if err != nil || m.InfoHash != f.meta.InfoHash {
		t.Fatalf("metadados recebidos divergem: %v", err)
	}
	if es, _ := os.ReadDir(dest); len(es) != 0 {
		t.Fatalf("NADA pode ser gravado antes de Download: %v", es)
	}
	if err := s.Download(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("download não terminou")
	}
	bad, verified, err := torrent.VerifyPieces(dest, m, nil)
	if err != nil || len(bad) != 0 || verified != len(m.Pieces) {
		t.Fatalf("verificação: bad=%v ok=%d/%d %v", bad, verified, len(m.Pieces), err)
	}
	sameContent(t, f, dest, nil)
	if d, total := s.Progress(); d != total || total != m.Total {
		t.Errorf("progresso %d/%d (esperado %d)", d, total, m.Total)
	}
	waitFor(t, "anúncio completed", 3*time.Second, func() bool { return tr.has("completed") })
	s.Stop()
	if !tr.has("started") || !tr.has("stopped") {
		t.Errorf("eventos do tracker: %v", tr.events)
	}
}

func TestFileSelectionDownloadsOnlySelectedFiles(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	tr := newTracker(t, f.ih, seeder.Addr().String())
	leech := testBackend(t, nil)
	dest := t.TempDir()
	s, _ := leech.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, dest)
	// .torrent sem trackers embebidos: dar o peer diretamente
	s.(*session).offerAddr(seeder.Addr().String())
	_ = tr
	sel := map[int]bool{0: true} // a.bin
	if err := s.Download(sel); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("download não terminou")
	}
	sameContent(t, f, dest, sel)
	bad, verified, err := torrent.VerifyPieces(dest, f.meta, sel)
	if err != nil || len(bad) != 0 || verified != 3 {
		t.Fatalf("peças inteiras de a.bin: bad=%v ok=%d %v", bad, verified, err)
	}
	if d, total := s.Progress(); d != total || total != 100_000 {
		t.Errorf("progresso %d/%d", d, total)
	}
	s.Stop()
}

func TestResumeKeepsVerifiedPiecesOnDisk(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	leech := testBackend(t, nil)
	dest := t.TempDir()
	// a.bin completo e b.bin só com metade: a retoma tem de aproveitar o que está certo
	pa := filepath.Join(dest, "pack", "a.bin")
	os.MkdirAll(filepath.Dir(pa), 0o755)
	da, _ := os.ReadFile(filepath.Join(f.src, "a.bin"))
	os.WriteFile(pa, da, 0o644)
	pb := filepath.Join(dest, "pack", "sub", "b.bin")
	os.MkdirAll(filepath.Dir(pb), 0o755)
	db, _ := os.ReadFile(filepath.Join(f.src, "sub", "b.bin"))
	os.WriteFile(pb, db[:30_000], 0o644)

	s, _ := leech.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, dest)
	if err := s.Download(nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Progress(); d < 98_304 {
		t.Fatalf("a retoma deveria reconhecer as peças 0-2 (>= 98304 bytes), reconheceu %d", d)
	}
	s.(*session).offerAddr(seeder.Addr().String())
	select {
	case <-s.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("download não terminou")
	}
	sameContent(t, f, dest, nil)
	s.Stop()
}

// ---- peer de referência com bytes literais (independente de wire.go) ----

type rawPeerMode int

const (
	rawHonest rawPeerMode = iota
	rawCorrupt
	rawOversize
	rawPoisonMeta
)

type rawPeer struct {
	ln   net.Listener
	f    fx
	mode rawPeerMode
	mu   sync.Mutex
	// observações
	servedBlocks int
	closedByUs   bool
}

func startRawPeer(t *testing.T, f fx, mode rawPeerMode) *rawPeer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &rawPeer{ln: ln, f: f, mode: mode}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *rawPeer) addr() string { return p.ln.Addr().String() }

func (p *rawPeer) serve(c net.Conn) {
	defer c.Close()
	hs := make([]byte, 68)
	if _, err := io.ReadFull(c, hs[:48]); err != nil || string(hs[1:20]) != "BitTorrent protocol" || !bytes.Equal(hs[28:48], p.f.ih[:]) {
		return
	}
	io.ReadFull(c, hs[48:]) // peer_id do leecher
	out := []byte("\x13BitTorrent protocol")
	reserved := make([]byte, 8)
	if p.mode == rawPoisonMeta {
		reserved[5] = 0x10
	}
	out = append(out, reserved...)
	out = append(out, p.f.ih[:]...)
	out = append(out, []byte("-RAW-0000000000000000")[:20]...)
	c.Write(out)

	switch p.mode {
	case rawOversize:
		c.Write([]byte{0x7F, 0xFF, 0xFF, 0xFF}) // "mensagem" de 2 GiB
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err := io.Copy(io.Discard, c) // o leecher tem de fechar a ligação
		p.mu.Lock()
		p.closedByUs = err == nil // EOF limpo = o outro lado fechou
		p.mu.Unlock()
		return
	case rawPoisonMeta:
		// handshake de extensão: {"m":{"ut_metadata":3},"metadata_size":20000}
		ext := []byte("d1:md11:ut_metadatai3ee13:metadata_sizei20000ee")
		c.Write(frame(append([]byte{20, 0}, ext...)))
		for {
			id, payload, err := rawRead(c)
			if err != nil {
				return
			}
			if id == 20 && len(payload) > 0 && payload[0] == 3 { // pedido ut_metadata dirigido ao nosso id 3
				hdr := []byte("d8:msg_typei1e5:piecei0e10:total_sizei20000ee")
				data := bytes.Repeat([]byte{0xEE}, 16384)
				c.Write(frame(append(append([]byte{20, 1}, hdr...), data...)))
				hdr1 := []byte("d8:msg_typei1e5:piecei1e10:total_sizei20000ee")
				c.Write(frame(append(append([]byte{20, 1}, hdr1...), bytes.Repeat([]byte{0xEE}, 3616)...)))
			}
		}
	}
	// bitfield: todas as peças (bits sobrantes a zero)
	n := len(p.f.meta.Pieces)
	bf := make([]byte, (n+7)/8)
	for i := 0; i < n; i++ {
		bf[i/8] |= 0x80 >> (uint(i) % 8)
	}
	c.Write(frame(append([]byte{5}, bf...)))
	for {
		id, payload, err := rawRead(c)
		if err != nil {
			return
		}
		switch id {
		case 2: // interested → unchoke
			c.Write([]byte{0, 0, 0, 1, 1})
		case 6: // request
			idx := int(binary.BigEndian.Uint32(payload[0:]))
			begin := int(binary.BigEndian.Uint32(payload[4:]))
			length := int(binary.BigEndian.Uint32(payload[8:]))
			off := idx*32768 + begin
			data := append([]byte(nil), p.f.stream[off:off+length]...)
			if p.mode == rawCorrupt {
				data[0] ^= 0xFF
			}
			msg := []byte{7}
			msg = binary.BigEndian.AppendUint32(msg, uint32(idx))
			msg = binary.BigEndian.AppendUint32(msg, uint32(begin))
			c.Write(frame(append(msg, data...)))
			p.mu.Lock()
			p.servedBlocks++
			p.mu.Unlock()
		}
	}
}

func frame(body []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

func rawRead(c net.Conn) (byte, []byte, error) {
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	var l [4]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n == 0 {
		return 255, nil, nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return 0, nil, err
	}
	return b[0], b[1:], nil
}

func TestDownloadFromIndependentReferencePeer(t *testing.T) {
	f := newFx(t)
	rp := startRawPeer(t, f, rawHonest)
	leech := testBackend(t, nil)
	dest := t.TempDir()
	s, _ := leech.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, dest)
	s.(*session).offerAddr(rp.addr())
	if err := s.Download(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("download não terminou com o peer de referência")
	}
	sameContent(t, f, dest, nil)
	s.Stop()
}

func TestCorruptPeerCannotPoisonDataHonestSeederStillCompletes(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	bad := startRawPeer(t, f, rawCorrupt)
	leech := testBackend(t, nil)
	dest := t.TempDir()
	s, _ := leech.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, dest)
	s.(*session).offerAddr(bad.addr())
	s.(*session).offerAddr(seeder.Addr().String())
	if err := s.Download(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("download não terminou")
	}
	// nenhuma peça corrompida chegou ao disco
	sameContent(t, f, dest, nil)
	bads, _, err := torrent.VerifyPieces(dest, f.meta, nil)
	if err != nil || len(bads) != 0 {
		t.Fatalf("peças inválidas no disco: %v %v", bads, err)
	}
	s.Stop()
}

func TestOnlyCorruptPeerNeverCompletesAndWritesNothingVerified(t *testing.T) {
	f := newFx(t)
	bad := startRawPeer(t, f, rawCorrupt)
	leech := testBackend(t, nil)
	dest := t.TempDir()
	s, _ := leech.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, dest)
	s.(*session).offerAddr(bad.addr())
	s.Download(nil)
	select {
	case <-s.Done():
		t.Fatal("não pode concluir só com dados corrompidos")
	case <-time.After(2 * time.Second):
	}
	if d, _ := s.Progress(); d != 0 {
		t.Fatalf("progresso %d com peer que só envia lixo", d)
	}
	s.Stop()
	bads, verified, _ := torrent.VerifyPieces(dest, f.meta, nil)
	if verified != 0 || len(bads) != len(f.meta.Pieces) {
		t.Fatalf("o disco não pode ter peças válidas: ok=%d bad=%v", verified, bads)
	}
}

func TestOversizedMessageDisconnectsPeer(t *testing.T) {
	f := newFx(t)
	rp := startRawPeer(t, f, rawOversize)
	leech := testBackend(t, nil)
	s, _ := leech.Add(context.Background(), torrentengine.Source{MetaInfo: f.raw}, t.TempDir())
	s.(*session).offerAddr(rp.addr())
	waitFor(t, "o leecher fechar a ligação", 6*time.Second, func() bool {
		rp.mu.Lock()
		defer rp.mu.Unlock()
		return rp.closedByUs
	})
	s.Stop()
}

func TestPoisonedMetadataIsRejectedByInfoHash(t *testing.T) {
	f := newFx(t)
	rp := startRawPeer(t, f, rawPoisonMeta)
	tr := newTracker(t, f.ih, rp.addr())
	leech := testBackend(t, nil)
	s, _ := leech.Add(context.Background(), magnetFor(f, tr.srv.URL), t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if raw, err := s.Meta(ctx); err == nil {
		t.Fatalf("metadados adulterados foram aceites (%d bytes)", len(raw))
	}
	s.Stop()
}

// ---- Fim a fim pelo torrentengine (política do produto) ----

type captureSink struct {
	mu sync.Mutex
	ev []engine.Event
}

func (c *captureSink) Emit(e engine.Event) { c.mu.Lock(); c.ev = append(c.ev, e); c.mu.Unlock() }
func (c *captureSink) find(k engine.EventKind) *engine.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.ev {
		if c.ev[i].Kind == k {
			e := c.ev[i]
			return &e
		}
	}
	return nil
}

func TestEngineEndToEndWithNativeBackend(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	tr := newTracker(t, f.ih, seeder.Addr().String())
	leech := testBackend(t, nil)
	eng, err := torrentengine.New(torrentengine.Options{Backend: leech, MetadataTimeout: 10 * time.Second,
		ProgressEvery: 20 * time.Millisecond, AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	dest := t.TempDir()
	src := magnetFor(f, tr.srv.URL)
	task := domain.DownloadTask{ID: "bt1", State: domain.StateRunning,
		Source: domain.DownloadSource{Kind: domain.SourceMagnet, URL: src.Magnet, InfoHash: f.meta.InfoHash},
		Policy: domain.DownloadPolicy{DestinationDir: dest, RequireIntegrity: domain.IntegrityTorrentPieces}}
	sink := &captureSink{}
	h, err := eng.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Completed", 25*time.Second, func() bool { return sink.find(engine.EventCompleted) != nil || sink.find(engine.EventError) != nil })
	if e := sink.find(engine.EventError); e != nil {
		t.Fatalf("erro: %+v", e.Error)
	}
	ev := sink.find(engine.EventCompleted)
	if ev.Payload["verified"] != "pieces" || ev.Payload["path"] != filepath.Join(dest, "pack") {
		t.Fatalf("Completed: %+v", ev.Payload)
	}
	sameContent(t, f, dest, nil)
	_ = h
}

// Um ficheiro "no meio" do torrent (nenhuma peça inteira dentro dele) só pode
// ser provado pelo Backend, que verificou cada peça antes de a gravar.
func TestEngineSingleSmallFileSelectionCompletesViaBackendProof(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	tr := newTracker(t, f.ih, seeder.Addr().String())
	leech := testBackend(t, nil)
	eng, _ := torrentengine.New(torrentengine.Options{Backend: leech, MetadataTimeout: 10 * time.Second,
		ProgressEvery: 20 * time.Millisecond, AllowPrivateNetworks: true})
	defer eng.Close()
	dest := t.TempDir()
	src := magnetFor(f, tr.srv.URL)
	task := domain.DownloadTask{ID: "bt2", State: domain.StateRunning,
		Source: domain.DownloadSource{Kind: domain.SourceMagnet, URL: src.Magnet, InfoHash: f.meta.InfoHash},
		Files:  []domain.FileMetadata{{Index: 1, Selected: true}}, // sub/b.bin: peças 3 e 4 cruzam a.bin e c.bin
		Policy: domain.DownloadPolicy{DestinationDir: dest, RequireIntegrity: domain.IntegrityTorrentPieces}}
	sink := &captureSink{}
	if _, err := eng.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Completed", 25*time.Second, func() bool { return sink.find(engine.EventCompleted) != nil || sink.find(engine.EventError) != nil })
	if e := sink.find(engine.EventError); e != nil {
		t.Fatalf("erro: %+v", e.Error)
	}
	if r := sink.find(engine.EventIntegrityReport); r == nil || r.Payload["proof"] != "backend" {
		t.Fatalf("a prova devia vir do backend: %+v", r)
	}
	sameContent(t, f, dest, map[int]bool{1: true})
}
