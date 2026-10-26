package hlsengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"streamvault/core/domain"
	"streamvault/core/engine"
)

// ---- servidor HLS de teste ----

type hlsOpts struct {
	nSegs, segSize int
	fmp4           bool
	jitter         bool // atrasos aleatórios por segmento (obriga a terminar fora de ordem)
	failFirst      map[int]bool
	notFound       int // índice de segmento que dá 404 (-1 = nenhum)
	requireReferer string
	maxConc        int
	key            string // "AES-128" => #EXT-X-KEY
	live           bool
	byterange      bool
	slowSeg0       time.Duration
	perSegBps      int
}

type hlsSrv struct {
	*httptest.Server
	o    hlsOpts
	segs [][]byte
	init []byte

	mu       sync.Mutex
	hits     map[string]int
	active   int32
	maxAct   int32
	rejected int32
	tried    map[string]bool
}

func newHLS(t *testing.T, o hlsOpts) *hlsSrv {
	t.Helper()
	if o.notFound == 0 {
		o.notFound = -1
	}
	h := &hlsSrv{o: o, hits: map[string]int{}, tried: map[string]bool{}}
	rnd := rand.New(rand.NewSource(42))
	for i := 0; i < o.nSegs; i++ {
		b := make([]byte, o.segSize+i*7) // tamanhos distintos
		rnd.Read(b)
		h.segs = append(h.segs, b)
	}
	if o.fmp4 {
		h.init = []byte("INIT-SEGMENT-fMP4-ftyp-moov")
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.handle))
	t.Cleanup(h.Close)
	return h
}

func (h *hlsSrv) hit(p string) int { h.mu.Lock(); defer h.mu.Unlock(); return h.hits[p] }
func (h *hlsSrv) totalSegHits() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for k, v := range h.hits {
		if strings.HasPrefix(k, "/seg/") {
			n += v
		}
	}
	return n
}

func (h *hlsSrv) media() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:4\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	if h.o.key != "" {
		fmt.Fprintf(&b, "#EXT-X-KEY:METHOD=%s,URI=\"key.bin\"\n", h.o.key)
	}
	if h.o.fmp4 {
		b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	}
	for i := range h.segs {
		if h.o.byterange {
			b.WriteString("#EXT-X-BYTERANGE:100@0\n")
		}
		ext := "ts"
		if h.o.fmp4 {
			ext = "m4s"
		}
		fmt.Fprintf(&b, "#EXTINF:4.0,\nseg/%d.%s?token=abc\n", i, ext)
	}
	if !h.o.live {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

func (h *hlsSrv) handle(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	h.mu.Lock()
	h.hits[p]++
	h.mu.Unlock()
	if h.o.requireReferer != "" && r.Header.Get("Referer") != h.o.requireReferer {
		http.Error(w, "hotlink", http.StatusForbidden)
		return
	}
	switch {
	case p == "/media.m3u8":
		w.Write([]byte(h.media()))
	case p == "/master.m3u8":
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=320x180\nlow.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720\nmedia.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=640x360\nmid.m3u8\n")
	case p == "/audio-master.m3u8":
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"pt\",URI=\"aud.m3u8\"\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=3000000,AUDIO=\"a\"\nmedia.m3u8\n")
	case p == "/aud.m3u8": // áudio em separado, em MPEG-TS: não se sabe juntar
		w.Write([]byte(h.media()))
	case p == "/low.m3u8" || p == "/mid.m3u8":
		http.Error(w, "variante errada", http.StatusTeapot)
	case p == "/init.mp4":
		w.Write(h.init)
	case strings.HasPrefix(p, "/seg/"):
		var idx int
		fmt.Sscanf(p, "/seg/%d", &idx)
		h.serveSeg(w, r, idx)
	default:
		http.NotFound(w, r)
	}
}

func (h *hlsSrv) serveSeg(w http.ResponseWriter, r *http.Request, idx int) {
	if h.o.maxConc > 0 {
		cur := atomic.AddInt32(&h.active, 1)
		defer atomic.AddInt32(&h.active, -1)
		if int(cur) > h.o.maxConc {
			atomic.AddInt32(&h.rejected, 1)
			w.Header().Set("Retry-After", "0")
			http.Error(w, "limite", http.StatusServiceUnavailable)
			return
		}
		for {
			m := atomic.LoadInt32(&h.maxAct)
			if cur <= m || atomic.CompareAndSwapInt32(&h.maxAct, m, cur) {
				break
			}
		}
	}
	if idx == h.o.notFound {
		http.NotFound(w, r)
		return
	}
	if h.o.failFirst[idx] {
		h.mu.Lock()
		first := !h.tried[r.URL.Path]
		h.tried[r.URL.Path] = true
		h.mu.Unlock()
		if first {
			w.Header().Set("Content-Length", fmt.Sprint(len(h.segs[idx])))
			w.Write(h.segs[idx][:len(h.segs[idx])/3]) // corta a meio
			panic(http.ErrAbortHandler)
		}
	}
	if idx == 0 && h.o.slowSeg0 > 0 {
		time.Sleep(h.o.slowSeg0)
	} else if h.o.jitter {
		time.Sleep(time.Duration((idx*37)%23) * time.Millisecond)
	}
	data := h.segs[idx]
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	if h.o.perSegBps > 0 {
		for off := 0; off < len(data); off += 16 << 10 {
			end := off + 16<<10
			if end > len(data) {
				end = len(data)
			}
			w.Write(data[off:end])
			time.Sleep(time.Duration(float64(end-off) / float64(h.o.perSegBps) * float64(time.Second)))
		}
		return
	}
	w.Write(data)
}

func (h *hlsSrv) concat() []byte {
	var b []byte
	b = append(b, h.init...)
	for _, s := range h.segs {
		b = append(b, s...)
	}
	return b
}

// ---- harness ----

type sink struct {
	mu sync.Mutex
	ev []engine.Event
}

func (s *sink) Emit(e engine.Event) { s.mu.Lock(); s.ev = append(s.ev, e); s.mu.Unlock() }
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
func (s *sink) count(k engine.EventKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.ev {
		if e.Kind == k {
			n++
		}
	}
	return n
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

func newEngine(mod func(*Options)) *Engine {
	o := Options{Connections: 4, SegmentBackoff: 5 * time.Millisecond, ProgressEvery: 20 * time.Millisecond}
	if mod != nil {
		mod(&o)
	}
	return New(o)
}

func hlsTask(id, url, dir string) domain.DownloadTask {
	return domain.DownloadTask{ID: id, State: domain.StateRunning,
		Source: domain.DownloadSource{Kind: domain.SourceHLS, URL: url, DisplayName: "video", Metadata: map[string]string{}},
		Policy: domain.DownloadPolicy{DestinationDir: dir, AllowResume: true}}
}

func runTask(t *testing.T, e *Engine, task domain.DownloadTask) (*sink, *job) {
	t.Helper()
	s := &sink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s})
	if err != nil {
		t.Fatal(err)
	}
	j := h.(*job)
	select {
	case <-j.done:
	case <-time.After(60 * time.Second):
		t.Fatal("timeout")
	}
	return s, j
}

func readFinal(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("ficheiro final %s: %v", name, err)
	}
	return b
}

func noLeftovers(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".svhls-") || strings.HasSuffix(e.Name(), ".svpart") {
			t.Errorf("sobrou lixo temporário: %s", e.Name())
		}
	}
}

// ---- testes ----

func TestVODDownloadConcatenatesSegmentsInOrderEvenWhenTheyFinishOutOfOrder(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 30, segSize: 20_000, jitter: true, slowSeg0: 120 * time.Millisecond})
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("h1", srv.URL+"/media.m3u8?sess=SEGREDO", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v (erro %+v)", j.err, s.lastErr())
	}
	got := readFinal(t, dir, "video.ts")
	if !bytes.Equal(got, srv.concat()) {
		t.Fatalf("a ordem/conteúdo dos segmentos diverge (%d vs %d bytes)", len(got), len(srv.concat()))
	}
	ev := s.find(engine.EventCompleted)
	want := sha256.Sum256(srv.concat())
	if ev.Payload["sha256"] != hex.EncodeToString(want[:]) {
		t.Errorf("SHA-256 do resultado errado: %s", ev.Payload["sha256"])
	}
	noLeftovers(t, dir)
	// os segmentos pedidos com query (token) funcionam e cada um é pedido 1x
	if n := srv.totalSegHits(); n != 30 {
		t.Errorf("esperava 30 pedidos de segmento, houve %d", n)
	}
}

func TestMasterPlaylistDownloadsTheBestVariant(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 6, segSize: 10_000})
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("h2", srv.URL+"/master.m3u8", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v (erro %+v)", j.err, s.lastErr())
	}
	if !bytes.Equal(readFinal(t, dir, "video.ts"), srv.concat()) {
		t.Fatal("conteúdo errado")
	}
	if srv.hit("/low.m3u8") != 0 || srv.hit("/mid.m3u8") != 0 {
		t.Error("só a melhor variante devia ser pedida")
	}
}

func TestFragmentedMP4InitSegmentComesFirstAndOutputIsMP4(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 8, segSize: 8_000, fmp4: true, jitter: true})
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("h3", srv.URL+"/media.m3u8", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v (erro %+v)", j.err, s.lastErr())
	}
	got := readFinal(t, dir, "video.mp4")
	if !bytes.HasPrefix(got, srv.init) || !bytes.Equal(got, srv.concat()) {
		t.Fatal("o segmento de inicialização tem de vir primeiro")
	}
}

func TestRefusesEncryptedLiveByteRangeAndSeparateAudioWithoutFetchingSegments(t *testing.T) {
	cases := []struct {
		name string
		o    hlsOpts
		path string
		msg  string
	}{
		{"cifrado AES-128", hlsOpts{nSegs: 3, segSize: 1000, key: "AES-128"}, "/media.m3u8", "cifrado"},
		{"cifrado SAMPLE-AES", hlsOpts{nSegs: 3, segSize: 1000, key: "SAMPLE-AES"}, "/media.m3u8", "cifrado"},
		{"ao vivo", hlsOpts{nSegs: 3, segSize: 1000, live: true}, "/media.m3u8", "ao vivo"},
		{"byte-range", hlsOpts{nSegs: 3, segSize: 1000, byterange: true}, "/media.m3u8", "BYTERANGE"},
		{"áudio separado", hlsOpts{nSegs: 3, segSize: 1000}, "/audio-master.m3u8", "áudio em separado"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newHLS(t, c.o)
			dir := t.TempDir()
			e := newEngine(nil)
			defer e.Close()
			s, j := runTask(t, e, hlsTask("hx", srv.URL+c.path, dir))
			de := s.lastErr()
			if j.err == nil || de == nil || de.Class != domain.ErrUnsupportedSource || !strings.Contains(de.Message, c.msg) || de.Action == "" {
				t.Fatalf("devia recusar com explicação (%q): %v %+v", c.msg, j.err, de)
			}
			if srv.totalSegHits() != 0 {
				t.Error("não deve descarregar nenhum segmento de um stream recusado")
			}
			if es, _ := os.ReadDir(dir); len(es) != 0 {
				t.Errorf("não deve deixar ficheiros: %v", es)
			}
		})
	}
}

func TestPauseResumeContinuesFromTheSyncedPointWithoutRefetchingAppendedSegments(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 40, segSize: 40_000, perSegBps: 2 << 20})
	dir := t.TempDir()
	e := newEngine(func(o *Options) { o.Connections = 3 })
	defer e.Close()
	task := hlsTask("h4", srv.URL+"/media.m3u8", dir)
	s1 := &sink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s1})
	if err != nil {
		t.Fatal(err)
	}
	j := h.(*job)
	for i := 0; atomic.LoadInt64(&j.bytesDone) < 600_000; i++ {
		if i > 1000 {
			t.Fatal("sem progresso")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.Pause(context.Background())
	<-j.done
	if j.err != nil || s1.count(engine.EventCompleted) != 0 {
		t.Fatalf("pausa não é erro nem conclusão: %v", j.err)
	}
	st := loadState(filepath.Join(dir, ".svhls-h4"))
	if st == nil || st.Appended < 5 || st.AppendedBytes <= 0 {
		t.Fatalf("o estado sincronizado devia guardar o ponto de retoma: %+v", st)
	}
	before := srv.totalSegHits()

	s2, j2 := runTask(t, e, task)
	if j2.err != nil || s2.count(engine.EventCompleted) != 1 {
		t.Fatalf("retoma: %v (erro %+v)", j2.err, s2.lastErr())
	}
	if !bytes.Equal(readFinal(t, dir, "video.ts"), srv.concat()) {
		t.Fatal("conteúdo diverge após pausa/retoma")
	}
	refetched := srv.totalSegHits() - before
	if max := 40 - st.Appended + 3*2; refetched > max { // só o que faltava (+ os em voo descartados)
		t.Errorf("a retoma voltou a pedir segmentos já anexados: %d pedidos (máx. esperado %d)", refetched, max)
	}
	noLeftovers(t, dir)
}

func TestChangedPlaylistRestartsInsteadOfMixingVersions(t *testing.T) {
	a := newHLS(t, hlsOpts{nSegs: 10, segSize: 5000})
	dir := t.TempDir()
	// simula um resto de uma execução anterior com OUTRA playlist
	work := filepath.Join(dir, ".svhls-h5")
	os.MkdirAll(work, 0o755)
	(&state{PlaylistSHA: "outra-playlist", N: 10, Name: "video.ts", Appended: 4, AppendedBytes: 99999}).save(work)
	os.WriteFile(filepath.Join(dir, "video.ts.svpart"), bytes.Repeat([]byte("X"), 99999), 0o644)
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("h5", a.URL+"/media.m3u8", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v (erro %+v)", j.err, s.lastErr())
	}
	if !bytes.Equal(readFinal(t, dir, "video.ts"), a.concat()) {
		t.Fatal("o resto de outra playlist contaminou o resultado")
	}
}

func TestTransientFailuresAreRetriedAndExpiredSegmentFailsClearly(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 12, segSize: 30_000, failFirst: map[int]bool{2: true, 7: true, 11: true}})
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("h6", srv.URL+"/media.m3u8", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("falhas intermitentes devem recuperar: %v (erro %+v)", j.err, s.lastErr())
	}
	if !bytes.Equal(readFinal(t, dir, "video.ts"), srv.concat()) {
		t.Fatal("conteúdo corrompido após as repetições")
	}

	gone := newHLS(t, hlsOpts{nSegs: 10, segSize: 3000, notFound: 4})
	dir2 := t.TempDir()
	s2, j2 := runTask(t, e, hlsTask("h6b", gone.URL+"/media.m3u8", dir2))
	de := s2.lastErr()
	if j2.err == nil || de == nil || de.Retryable || de.Action == "" || s2.count(engine.EventCompleted) != 0 {
		t.Fatalf("404 num segmento devia falhar com explicação: %v %+v", j2.err, de)
	}
	if _, err := os.Stat(filepath.Join(dir2, "video.ts")); err == nil {
		t.Error("não pode existir ficheiro final incompleto")
	}
}

func TestRefererAndUserAgentReachPlaylistAndSegments(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 5, segSize: 4000, requireReferer: "https://site.example.com/aula"})
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	// sem Referer: o servidor recusa
	s0, j0 := runTask(t, e, hlsTask("h7a", srv.URL+"/media.m3u8", t.TempDir()))
	if j0.err == nil || s0.lastErr() == nil || s0.lastErr().Class != domain.ErrURLExpired {
		t.Fatalf("sem referer devia falhar como acesso recusado: %v", j0.err)
	}
	task := hlsTask("h7b", srv.URL+"/media.m3u8", dir)
	task.Source.Metadata["referer"] = "https://site.example.com/aula#t=5"
	s, j := runTask(t, e, task)
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("com referer devia concluir: %v (erro %+v)", j.err, s.lastErr())
	}
}

func TestPublicPlaylistCannotPointSegmentsAtTheInternalNetwork(t *testing.T) {
	// playlist servida por uma origem "pública" (desvio de dial) cujos segmentos
	// apontam para 127.0.0.1: tem de ser recusada ANTES de qualquer pedido de segmento
	hit := int32(0)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
		w.Write([]byte("SEGREDO-INTERNO"))
	}))
	defer internal.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#EXTM3U\n#EXTINF:4,\n%s/interno.ts\n#EXT-X-ENDLIST\n", internal.URL)
	}))
	defer outer.Close()
	outerAddr := strings.TrimPrefix(outer.URL, "http://")
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, n, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "93.184.216.34") {
			addr = outerAddr
		}
		return (&net.Dialer{}).DialContext(ctx, n, addr)
	}}}
	e := newEngine(func(o *Options) { o.Client = client })
	defer e.Close()
	dir := t.TempDir()
	s, j := runTask(t, e, hlsTask("h8", "http://93.184.216.34/p.m3u8", dir))
	de := s.lastErr()
	if j.err == nil || de == nil || de.Class != domain.ErrPolicyViolation {
		t.Fatalf("devia recusar por segurança: %v %+v", j.err, de)
	}
	if atomic.LoadInt32(&hit) != 0 {
		t.Fatal("a rede interna foi contactada")
	}
}

func TestAdaptsWhenServerLimitsConcurrentRequests(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 24, segSize: 20_000, maxConc: 2, perSegBps: 4 << 20})
	dir := t.TempDir()
	e := newEngine(func(o *Options) { o.Connections = 8 })
	defer e.Close()
	s, j := runTask(t, e, hlsTask("h9", srv.URL+"/media.m3u8", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v (erro %+v)", j.err, s.lastErr())
	}
	if !bytes.Equal(readFinal(t, dir, "video.ts"), srv.concat()) {
		t.Fatal("conteúdo errado")
	}
	if atomic.LoadInt32(&srv.rejected) == 0 {
		t.Error("o teste não exerceu o limite")
	}
	if m := atomic.LoadInt32(&srv.maxAct); m > 2 {
		t.Errorf("servidor só aceita 2, aceitou %d", m)
	}
}

func TestIntegrityPolicyAndNameCollision(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 5, segSize: 5000})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "video.ts"), []byte("existente"), 0o644)
	e := newEngine(nil)
	defer e.Close()
	sum := sha256.Sum256(srv.concat())
	task := hlsTask("h10", srv.URL+"/media.m3u8", dir)
	task.Policy.RequireIntegrity = domain.IntegritySHA256
	task.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: hex.EncodeToString(sum[:])}}
	s, j := runTask(t, e, task)
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v (erro %+v)", j.err, s.lastErr())
	}
	if b := readFinal(t, dir, "video.ts"); string(b) != "existente" {
		t.Fatal("sobrescreveu um ficheiro existente")
	}
	if !bytes.Equal(readFinal(t, dir, "video (1).ts"), srv.concat()) {
		t.Fatal("o novo ficheiro devia ficar em 'video (1).ts'")
	}
	// hash esperado errado
	bad := hlsTask("h10b", srv.URL+"/media.m3u8", t.TempDir())
	bad.Policy.RequireIntegrity = domain.IntegritySHA256
	bad.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: strings.Repeat("0", 64)}}
	s2, j2 := runTask(t, e, bad)
	if j2.err == nil || s2.lastErr() == nil || s2.lastErr().Class != domain.ErrIntegrityFailure || s2.count(engine.EventCompleted) != 0 {
		t.Fatalf("hash errado devia falhar a integridade: %v", j2.err)
	}
}

func TestSpeedLimitIsAggregateAndProgressReportsEstimate(t *testing.T) {
	srv := newHLS(t, hlsOpts{nSegs: 20, segSize: 50_000})
	dir := t.TempDir()
	e := newEngine(func(o *Options) { o.Connections = 6 })
	defer e.Close()
	task := hlsTask("h11", srv.URL+"/media.m3u8", dir)
	task.Policy.SpeedLimitBps = 400_000 // ~1 MB total => ≥ ~2 s
	t0 := time.Now()
	s, j := runTask(t, e, task)
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("%v", j.err)
	}
	if d := time.Since(t0); d < 1500*time.Millisecond {
		t.Errorf("limite agregado não aplicado: %v", d)
	}
	if s.count(engine.EventProgress) < 2 {
		t.Error("devia haver progresso")
	}
	if !e.Capabilities().SupportsPause || e.CanHandle(domain.DownloadSource{Kind: domain.SourceHTTP}) || !e.CanHandle(domain.DownloadSource{Kind: domain.SourceHLS}) {
		t.Error("capacidades/CanHandle incorretos")
	}
}

func TestOutputExtensionFollowsSegmentType(t *testing.T) {
	mk := func(urls ...string) *Playlist {
		p := &Playlist{}
		for _, u := range urls {
			p.Segments = append(p.Segments, Segment{URL: u})
		}
		return p
	}
	if outputExt(mk("https://a/x.ts", "https://a/y.ts")) != ".ts" || outputExt(mk("https://a/x.aac")) != ".aac" ||
		outputExt(mk("https://a/x.m4s?q=1")) != ".mp4" || outputExt(&Playlist{MapURL: "https://a/i.mp4", Segments: mk("https://a/x.ts").Segments}) != ".mp4" ||
		outputExt(mk("https://a/x", "https://a/y.ts")) != ".ts" {
		t.Error("extensão de saída incorreta")
	}
}
