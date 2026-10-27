package httpengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"streamvault/core/domain"
	"streamvault/core/engine"
)

// ---- servidor de teste com controlo fino ----

type srvOpts struct {
	perConnBps    int   // limite por ligação (0 = sem limite)
	noRanges      bool  // não anuncia nem honra Range (corpo completo)
	liar          bool  // anuncia Accept-Ranges mas ignora Range (200)
	maxConns      int   // ligações simultâneas aceites (0 = ilimitado); as outras recebem 503
	driftAfter    int   // após N pedidos Range, o ETag muda
	cutFirstTries bool  // a 1.ª tentativa de cada segmento é cortada a meio
	lateReject    int64 // o pedido Range que começa neste offset leva 503 (1x) SÓ depois de 2 outros terminarem
}

type testSrv struct {
	*httptest.Server
	data []byte
	o    srvOpts

	mu                          sync.Mutex
	requests                    int
	ranges                      []string
	bytesOut                    int64
	rejected                    int32
	active                      int32
	maxActive                   int32
	rangeActive, maxRangeActive int32
	rangeFinished               int32
	lateRejected                int32
	seen                        map[string]bool
}

type slowWriter struct {
	http.ResponseWriter
	bps int
	out *int64
}

func (w *slowWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		c := 16 << 10
		if c > len(p) {
			c = len(p)
		}
		n, err := w.ResponseWriter.Write(p[:c])
		written += n
		atomic.AddInt64(w.out, int64(n))
		if err != nil {
			return written, err
		}
		if fl, ok := w.ResponseWriter.(http.Flusher); ok {
			fl.Flush()
		}
		if w.bps > 0 {
			time.Sleep(time.Duration(float64(n) / float64(w.bps) * float64(time.Second)))
		}
		p = p[c:]
	}
	return written, nil
}

func newTestSrv(t *testing.T, size int, o srvOpts) *testSrv {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	s := &testSrv{data: data, o: o, seen: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *testSrv) handle(w http.ResponseWriter, r *http.Request) {
	rng := r.Header.Get("Range")
	s.mu.Lock()
	s.requests++
	s.ranges = append(s.ranges, rng)
	n := s.requests
	s.mu.Unlock()

	if s.o.maxConns > 0 {
		cur := atomic.AddInt32(&s.active, 1)
		defer atomic.AddInt32(&s.active, -1)
		if int(cur) > s.o.maxConns {
			atomic.AddInt32(&s.rejected, 1)
			w.Header().Set("Retry-After", "0")
			http.Error(w, "demasiadas ligações", http.StatusServiceUnavailable)
			return
		}
		for {
			m := atomic.LoadInt32(&s.maxActive)
			if cur <= m || atomic.CompareAndSwapInt32(&s.maxActive, m, cur) {
				break
			}
		}
	}
	if rng != "" {
		cur := atomic.AddInt32(&s.rangeActive, 1)
		defer atomic.AddInt32(&s.rangeActive, -1)
		defer atomic.AddInt32(&s.rangeFinished, 1)
		if s.o.lateReject > 0 {
			var a int64
			fmt.Sscanf(rng, "bytes=%d-", &a)
			if a == s.o.lateReject && atomic.CompareAndSwapInt32(&s.lateRejected, 0, 1) {
				for i := 0; atomic.LoadInt32(&s.rangeFinished) < 2 && i < 2000; i++ { // espera os outros terminarem
					time.Sleep(2 * time.Millisecond)
				}
				w.Header().Set("Retry-After", "0")
				http.Error(w, "limite tardio", http.StatusServiceUnavailable)
				return
			}
		}
		for {
			m := atomic.LoadInt32(&s.maxRangeActive)
			if cur <= m || atomic.CompareAndSwapInt32(&s.maxRangeActive, m, cur) {
				break
			}
		}
	}
	etag := `"v1"`
	if s.o.driftAfter > 0 && n > s.o.driftAfter {
		etag = `"v2"`
	}
	w.Header().Set("ETag", etag)
	sw := &slowWriter{ResponseWriter: w, bps: s.o.perConnBps, out: &s.bytesOut}

	switch {
	case s.o.noRanges:
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		sw.Write(s.data)
	case s.o.liar:
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		sw.Write(s.data)
	default:
		if s.o.cutFirstTries && strings.HasPrefix(rng, "bytes=") {
			s.mu.Lock()
			first := !s.seen[rng]
			s.seen[rng] = true
			s.mu.Unlock()
			if first {
				var a, b int
				fmt.Sscanf(rng, "bytes=%d-%d", &a, &b)
				if a%(64<<10) == 0 && b-a > 1000 { // só a 1.ª tentativa (início do segmento)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(s.data)))
					w.Header().Set("Content-Length", strconv.Itoa(b-a+1))
					w.WriteHeader(http.StatusPartialContent)
					sw.Write(s.data[a : a+(b-a)*4/10]) // 40% e corta a ligação
					panic(http.ErrAbortHandler)
				}
			}
		}
		http.ServeContent(sw, r, "f.bin", time.Unix(0, 0), bytes.NewReader(s.data))
	}
}

func (s *testSrv) reqCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }

// ---- helpers de execução ----

func segEngine(opts ...func(*Options)) (*Engine, *resumeStore) {
	rs := &resumeStore{m: map[string]ResumeState{}}
	o := Options{Connections: 8, MinSegmentBytes: 64 << 10, SegmentBackoff: 5 * time.Millisecond,
		ProgressEvery: 20 * time.Millisecond, ResumeProvider: rs.get, ResumePersist: rs.put}
	for _, f := range opts {
		f(&o)
	}
	return New(o), rs
}

type resumeStore struct {
	mu sync.Mutex
	m  map[string]ResumeState
}

func (r *resumeStore) get(id string) (*ResumeState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[id]
	if !ok {
		return nil, false
	}
	cp := v
	cp.SegDone = append([]int64(nil), v.SegDone...)
	return &cp, true
}
func (r *resumeStore) put(rs ResumeState) { r.mu.Lock(); r.m[rs.TaskID] = rs; r.mu.Unlock() }

func runTask(t *testing.T, e *Engine, task domain.DownloadTask) (*recSink, *job, time.Duration) {
	t.Helper()
	s := &recSink{}
	t0 := time.Now()
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
	return s, j, time.Since(t0)
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func mustContent(t *testing.T, dir, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("conteúdo de %s diverge (err=%v, %d/%d bytes)", name, err, len(got), len(want))
	}
}

// ---- 1. O ponto da decisão: mais rápido que uma ligação, medido ----

func TestSegmentedIsMuchFasterThanSingleConnectionWhenServerThrottlesPerConnection(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{perConnBps: 1 << 20}) // ~1 MiB/s por ligação
	dl1, dl8 := t.TempDir(), t.TempDir()

	e1, _ := segEngine(func(o *Options) { o.Connections = 1 })
	defer e1.Close()
	s1, j1, d1 := runTask(t, e1, mkTask(srv.URL+"/f.bin", dl1))
	if j1.err != nil || s1.count(engine.EventCompleted) != 1 {
		t.Fatalf("1 ligação: %v", j1.err)
	}
	mustContent(t, dl1, "f.bin", srv.data)

	srv.mu.Lock()
	srv.requests, srv.ranges = 0, nil
	srv.mu.Unlock()
	e8, _ := segEngine()
	defer e8.Close()
	s8, j8, d8 := runTask(t, e8, mkTask(srv.URL+"/f.bin", dl8))
	if j8.err != nil || s8.count(engine.EventCompleted) != 1 {
		t.Fatalf("8 ligações: %v", j8.err)
	}
	mustContent(t, dl8, "f.bin", srv.data)

	speedup := float64(d1) / float64(d8)
	t.Logf("1 ligação: %v · 8 ligações: %v · aceleração ×%.1f (%d pedidos)", d1.Round(time.Millisecond), d8.Round(time.Millisecond), speedup, srv.reqCount())
	if speedup < 3.0 {
		t.Fatalf("a segmentação devia acelerar pelo menos 3×, acelerou ×%.1f", speedup)
	}
	if srv.reqCount() < 2 {
		t.Error("o servidor devia ter visto várias ligações")
	}
}

func TestSegmentedCompletionReportsHashAndVerifiesExpectedSHA256(t *testing.T) {
	srv := newTestSrv(t, 1<<20, srvOpts{})
	e, _ := segEngine()
	defer e.Close()
	task := mkTask(srv.URL+"/f.bin", t.TempDir())
	task.Policy.RequireIntegrity = domain.IntegritySHA256
	task.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: sha(srv.data)}}
	s, j, _ := runTask(t, e, task)
	if j.err != nil {
		t.Fatal(j.err)
	}
	ev := s.find(engine.EventCompleted)
	if ev == nil || ev.Payload["sha256"] != sha(srv.data) {
		t.Fatalf("Completed devia trazer o SHA-256 do ficheiro: %+v", ev)
	}
	// hash esperado errado: falha de integridade e o ficheiro final NÃO é criado
	bad := mkTask(srv.URL+"/f.bin", t.TempDir())
	bad.Policy.RequireIntegrity = domain.IntegritySHA256
	bad.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: strings.Repeat("0", 64)}}
	s2, j2, _ := runTask(t, e, bad)
	de := s2.lastErr()
	if j2.err == nil || de == nil || de.Class != domain.ErrIntegrityFailure || s2.count(engine.EventCompleted) != 0 {
		t.Fatalf("hash errado deve falhar a integridade: %v %+v", j2.err, de)
	}
}

// ---- 2. Pausa e retoma por segmento ----

func TestSegmentedPauseResumeKeepsPerSegmentProgress(t *testing.T) {
	srv := newTestSrv(t, 4<<20, srvOpts{perConnBps: 1 << 20})
	dir := t.TempDir()
	e, store := segEngine(func(o *Options) { o.Connections = 4; o.MinSegmentBytes = 256 << 10 })
	defer e.Close()
	task := mkTask(srv.URL+"/f.bin", dir)

	s1 := &recSink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s1})
	if err != nil {
		t.Fatal(err)
	}
	j := h.(*job)
	for i := 0; atomic.LoadInt64(&j.bytesDone) < 1<<20; i++ {
		if i > 500 {
			t.Fatal("sem progresso")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.Pause(context.Background())
	<-j.done
	if j.err != nil {
		t.Fatalf("pausa não é erro: %v", j.err)
	}
	rs, ok := store.get(task.ID)
	if !ok || rs.SegSize <= 0 || len(rs.SegDone) < 8 {
		t.Fatalf("estado por segmento em falta: %+v", rs)
	}
	var sum int64
	for _, d := range rs.SegDone {
		sum += d
	}
	if sum != rs.BytesDone || sum < 1<<20 {
		t.Fatalf("soma dos segmentos %d ≠ BytesDone %d (esperado ≥ 1 MiB)", sum, rs.BytesDone)
	}
	if fi, err := os.Stat(rs.TempPath); err != nil || fi.Size() != int64(len(srv.data)) {
		t.Fatalf("o temporário devia estar pré-alocado com o tamanho total: %v %v", err, fi)
	}
	// retoma: novo motor, mesmo estado
	srv.mu.Lock()
	srv.ranges = nil
	srv.mu.Unlock()
	atomic.StoreInt64(&srv.bytesOut, 0)
	e2, _ := segEngine(func(o *Options) {
		o.Connections = 4
		o.MinSegmentBytes = 256 << 10
		o.ResumeProvider = store.get
		o.ResumePersist = store.put
	})
	defer e2.Close()
	s2, j2, _ := runTask(t, e2, task)
	if j2.err != nil || s2.count(engine.EventCompleted) != 1 {
		t.Fatalf("retoma: %v", j2.err)
	}
	mustContent(t, dir, "f.bin", srv.data)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.ranges) == 0 || srv.ranges[0] != "bytes=0-0" {
		t.Errorf("a retoma deve começar com a sonda 0-0 (validar identidade): %v", srv.ranges)
	}
	if out, limit := atomic.LoadInt64(&srv.bytesOut), int64(len(srv.data))-rs.BytesDone+1<<20; out > limit {
		t.Errorf("a retoma voltou a descarregar bytes já feitos: serviu %d (limite %d)", out, limit)
	}
}

func TestInvalidSegmentResumeStateRestartsCleanly(t *testing.T) {
	srv := newTestSrv(t, 1<<20, srvOpts{})
	dir := t.TempDir()
	tmp := filepath.Join(dir, "f.bin.svpart")
	os.WriteFile(tmp, make([]byte, 1<<20), 0o644)
	e, store := segEngine()
	defer e.Close()
	task := mkTask(srv.URL+"/f.bin", dir)
	for _, bad := range []ResumeState{
		{TaskID: task.ID, TempPath: tmp, Total: 1 << 20, BytesDone: 100, SegSize: 262144, SegDone: []int64{100}},                // contagem errada
		{TaskID: task.ID, TempPath: tmp, Total: 1 << 20, BytesDone: 999999, SegSize: 262144, SegDone: []int64{999999, 0, 0, 0}}, // segmento a mais
		{TaskID: task.ID, TempPath: tmp, Total: 1 << 19, BytesDone: 100, SegSize: 262144, SegDone: []int64{100, 0}},             // total ≠ ficheiro
	} {
		store.put(bad)
		s, j, _ := runTask(t, e, task)
		if j.err != nil || s.count(engine.EventCompleted) != 1 {
			t.Fatalf("estado inválido devia levar a reinício limpo: %v", j.err)
		}
		mustContent(t, dir, "f.bin", srv.data)
		os.Remove(filepath.Join(dir, "f.bin"))
		os.WriteFile(tmp, make([]byte, 1<<20), 0o644)
	}
}

// ---- 3. Segurança dos dados: o recurso muda a meio ----

func TestResourceChangedMidDownloadAbortsInsteadOfMixingVersions(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{perConnBps: 2 << 20, driftAfter: 3})
	dir := t.TempDir()
	e, store := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	de := s.lastErr()
	if j.err == nil || de == nil || de.Class != domain.ErrResumeMismatch || s.count(engine.EventCompleted) != 0 {
		t.Fatalf("mudança do ficheiro devia abortar com ErrResumeMismatch: %v %+v", j.err, de)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.bin")); err == nil {
		t.Error("não pode existir ficheiro final misturando versões")
	}
	if rs, ok := store.get("t1"); ok && rs.BytesDone > 0 {
		t.Errorf("progresso de uma versão antiga não pode ficar para retoma: %+v", rs)
	}
}

// ---- 4. Servidores que não suportam (ou mentem sobre) Range ----

func TestServerWithoutRangeUsesSingleConnection(t *testing.T) {
	srv := newTestSrv(t, 1<<20, srvOpts{noRanges: true})
	dir := t.TempDir()
	e, _ := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatal(j.err)
	}
	mustContent(t, dir, "f.bin", srv.data)
	if n := srv.reqCount(); n != 1 {
		t.Errorf("sem Range devia usar 1 só pedido, usou %d", n)
	}
}

func TestServerThatLiesAboutRangeFallsBackToSingleAndStillCompletes(t *testing.T) {
	srv := newTestSrv(t, 1<<20, srvOpts{liar: true})
	dir := t.TempDir()
	e, _ := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("o fallback devia concluir: %v", j.err)
	}
	mustContent(t, dir, "f.bin", srv.data)
	if fs, _ := filepath.Glob(filepath.Join(dir, "*.svpart")); len(fs) != 0 {
		t.Errorf("sobrou temporário: %v", fs)
	}
}

func TestSmallFilesStayOnSingleConnection(t *testing.T) {
	srv := newTestSrv(t, 100<<10, srvOpts{}) // < 2 × 64 KiB
	dir := t.TempDir()
	e, _ := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 || srv.reqCount() != 1 {
		t.Fatalf("ficheiro pequeno: err=%v pedidos=%d", j.err, srv.reqCount())
	}
}

// ---- 5. Adaptação a servidores que limitam ligações ----

func TestAdaptsWhenServerAcceptsOnlyTwoConnections(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{perConnBps: 2 << 20, maxConns: 2})
	dir := t.TempDir()
	e, _ := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("devia adaptar-se ao limite do servidor: %v", j.err)
	}
	mustContent(t, dir, "f.bin", srv.data)
	if atomic.LoadInt32(&srv.rejected) == 0 {
		t.Error("o teste não exerceu o limite de ligações")
	}
	if m := atomic.LoadInt32(&srv.maxActive); m > 2 {
		t.Errorf("o servidor só aceita 2 em simultâneo, aceitou %d", m)
	}
}

func TestSingleConnectionServerRefusingAllStillFailsClearly(t *testing.T) {
	// o servidor responde sempre 503: tem de falhar com diagnóstico, não ficar preso
	always503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		http.Error(w, "indisponível", http.StatusServiceUnavailable)
	}))
	defer always503.Close()
	e, _ := segEngine()
	defer e.Close()
	s, j, d := runTask(t, e, mkTask(always503.URL+"/f.bin", t.TempDir()))
	if j.err == nil || s.lastErr() == nil || s.count(engine.EventCompleted) != 0 {
		t.Fatalf("devia falhar: %v", j.err)
	}
	if d > 20*time.Second {
		t.Errorf("demorou demasiado a desistir: %v", d)
	}
}

// ---- 6. Ligações que caem a meio ----

func TestFlakyConnectionsResumeInsideTheSegment(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{cutFirstTries: true})
	dir := t.TempDir()
	e, _ := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("devia recuperar das quedas: %v", j.err)
	}
	mustContent(t, dir, "f.bin", srv.data) // nenhum byte corrompido ou em falta
	// houve pedidos que começam a meio de um segmento (retoma dentro do segmento)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	mid := 0
	for _, r := range srv.ranges {
		var a, b int
		if n, _ := fmt.Sscanf(r, "bytes=%d-%d", &a, &b); n == 2 && a%(64<<10) != 0 {
			mid++
		}
	}
	if mid == 0 {
		t.Errorf("esperava pedidos a meio de segmentos após as quedas: %v", srv.ranges)
	}
}

// ---- 7. Limite de velocidade agregado ----

func TestSpeedLimitAppliesToTheSumOfAllConnections(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{})
	dir := t.TempDir()
	e, _ := segEngine()
	defer e.Close()
	task := mkTask(srv.URL+"/f.bin", dir)
	task.Policy.SpeedLimitBps = 1 << 20 // 1 MiB/s no total, com 8 ligações
	s, j, d := runTask(t, e, task)
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatal(j.err)
	}
	mustContent(t, dir, "f.bin", srv.data)
	if d < 1500*time.Millisecond {
		t.Errorf("2 MiB a 1 MiB/s agregado não pode demorar %v (o limite não está a ser partilhado)", d)
	}
	if d > 6*time.Second {
		t.Errorf("limite demasiado agressivo: %v", d)
	}
}

// ---- 8. Política e concorrência ----

func TestPolicyMaxConcurrencyOverridesEngineDefault(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{perConnBps: 2 << 20})
	dir := t.TempDir()
	e, _ := segEngine() // 8 por defeito
	defer e.Close()
	task := mkTask(srv.URL+"/f.bin", dir)
	task.Policy.MaxConcurrency = 3
	s, j, _ := runTask(t, e, task)
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatal(j.err)
	}
	if m := atomic.LoadInt32(&srv.maxRangeActive); m > 3 || m < 2 {
		t.Errorf("MaxConcurrency=3: o servidor devia ver 2-3 ligações com Range em simultâneo, viu %d", m)
	}
}

func TestSegmentedReportsProgressAndSpeed(t *testing.T) {
	srv := newTestSrv(t, 2<<20, srvOpts{perConnBps: 1 << 20})
	e, _ := segEngine()
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", t.TempDir()))
	if j.err != nil {
		t.Fatal(j.err)
	}
	if s.count(engine.EventProgress) < 2 {
		t.Errorf("devia haver eventos de progresso: %d", s.count(engine.EventProgress))
	}
	if !e.Capabilities().SupportsSegmentation {
		t.Error("a capacidade de segmentação devia estar declarada")
	}
}

func TestPlanSegSizeBounds(t *testing.T) {
	if got := planSegSize(10<<20, 8, 1<<20); got != 1<<20 {
		t.Errorf("mínimo: %d", got)
	}
	if got := planSegSize(100<<30, 8, 1<<20); got != maxSegmentSize {
		t.Errorf("máximo: %d", got)
	}
	st := newSegState(10, 4)
	if st.count() != 3 || st.length(2) != 2 {
		t.Errorf("divisão: %d segmentos, último com %d", st.count(), st.length(2))
	}
	if a, b := st.bounds(1); a != 4 || b != 7 {
		t.Errorf("bounds: %d-%d", a, b)
	}
}

func (s *recSink) find(k engine.EventKind) *engine.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.events {
		if s.events[i].Kind == k {
			e := s.events[i]
			return &e
		}
	}
	return nil
}

// Um trabalhador que se reforma DEPOIS de os outros terminarem devolve o segmento
// à fila: tem de haver uma nova ronda, senão o segmento fica órfão ("N/M bytes").
func TestRetiredWorkerReturningSegmentAfterOthersFinishedStillCompletes(t *testing.T) {
	const seg = 64 << 10
	srv := newTestSrv(t, 3*seg, srvOpts{lateReject: 2 * seg})
	dir := t.TempDir()
	e, _ := segEngine(func(o *Options) { o.Connections = 3 })
	defer e.Close()
	s, j, _ := runTask(t, e, mkTask(srv.URL+"/f.bin", dir))
	if atomic.LoadInt32(&srv.lateRejected) != 1 {
		t.Fatal("o teste não exerceu o cenário (o 503 tardio não ocorreu)")
	}
	if j.err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("segmento devolvido tardiamente ficou órfão: %v (erro: %+v)", j.err, s.lastErr())
	}
	mustContent(t, dir, "f.bin", srv.data)
}
