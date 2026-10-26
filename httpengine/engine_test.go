package httpengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// sink de teste thread-safe.
type recSink struct {
	mu     sync.Mutex
	events []engine.Event
}

func (s *recSink) Emit(e engine.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recSink) count(k engine.EventKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (s *recSink) lastErr() *domain.DownloadError {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last *domain.DownloadError
	for _, e := range s.events {
		if e.Kind == engine.EventError && e.Error != nil {
			last = e.Error
		}
	}
	return last
}

func mkTask(url, dir string) domain.DownloadTask {
	return domain.DownloadTask{
		ID:     fmt.Sprintf("t-%d", time.Now().UnixNano()),
		Source: domain.DownloadSource{Kind: domain.SourceHTTP, URL: url},
		Policy: domain.DownloadPolicy{DestinationDir: dir},
		State:  domain.StatePending,
	}
}

func runToCompletion(t *testing.T, e *Engine, task domain.DownloadTask, sink *recSink) {
	t.Helper()
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := h.(*job)
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout aguardando transferência")
	}
	if j.err != nil {
		t.Fatalf("erro na transferência: %v", j.err)
	}
}

// --- Teste 1: servidor COM suporte a Range — retoma real após pausa ---

func TestRangeServerResume(t *testing.T) {
	payload := make([]byte, 300*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var mu sync.Mutex
	gotRanges := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if rg := r.Header.Get("Range"); rg != "" {
			gotRanges = append(gotRanges, rg)
		}
		mu.Unlock()
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"abc123"`)
		http.ServeContent(w, r, "f.bin", time.Unix(0, 0), strings.NewReader(string(payload)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	resumes := map[string]*ResumeState{}
	var rmu sync.Mutex
	// Pausa DETERMINÍSTICA via WriteHook: pausamos imediatamente após a
	// primeira escrita confirmada em disco. Redes loopback são rápidas
	// demais para pausa por tempo; este método garante que a pausa ocorre
	// com progresso persistido e NUNCA a meio de uma escrita.
	var once sync.Once
	pausedCh := make(chan struct{})
	e := New(Options{
		ProgressEvery: 10 * time.Millisecond,
		WriteHook: func(taskID string, offset int64) {
			once.Do(func() {
				close(pausedCh)
				testSetPauseHook(taskID) // pausa efetiva na PRÓXIMA fronteira de escrita segura
			})
		},
		ResumeProvider: func(id string) (*ResumeState, bool) {
			rmu.Lock()
			defer rmu.Unlock()
			rs, ok := resumes[id]
			return rs, ok
		},
		ResumePersist: func(rs ResumeState) {
			rmu.Lock()
			defer rmu.Unlock()
			cp := rs
			resumes[rs.TaskID] = &cp
		},
	})
	defer e.Close()

	task := mkTask(srv.URL+"/f.bin", dir)
	sink := &recSink{}

	// Start; a pausa acontece dentro do WriteHook (após chunk escrito).
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	j := h.(*job)
	select {
	case <-pausedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteHook nunca disparou — transferência não avançou?")
	}
	<-j.done
	partial := atomicLoad(&j.bytesDone)
	if partial == 0 || partial >= int64(len(payload)) {
		t.Fatalf("pausa em ponto inesperado: %d bytes", partial)
	}
	// temp deve existir com exatamente partial bytes
	rmu.Lock()
	rs := resumes[task.ID]
	rmu.Unlock()
	if rs == nil || rs.TempPath == "" {
		t.Fatal("estado de retoma não persistido")
	}
	st, serr := os.Stat(rs.TempPath)
	if serr != nil || st.Size() != partial {
		t.Fatalf("temp inconsistente: stat=%v size esperado %d", serr, partial)
	}

	// Retomar: mesmo ID, motor deve enviar Range e completar
	task2 := task
	sink2 := &recSink{}
	runToCompletion(t, e, task2, sink2)

	mu.Lock()
	hadRange := len(gotRanges) > 0 && strings.HasPrefix(gotRanges[len(gotRanges)-1], fmt.Sprintf("bytes=%d-", partial))
	mu.Unlock()
	if !hadRange {
		t.Fatalf("retoma não enviou Range correto; ranges=%v offset=%d", gotRanges, partial)
	}
	final := filepath.Join(dir, "f.bin")
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ficheiro final ausente: %v", err)
	}
	if len(data) != len(payload) {
		t.Fatalf("tamanho final %d != %d", len(data), len(payload))
	}
	wantSum := sha256.Sum256(payload)
	gotSum := sha256.Sum256(data)
	if hex.EncodeToString(wantSum[:]) != hex.EncodeToString(gotSum[:]) {
		t.Fatal("CONTEÚDO CORROMPIDO na retoma — bytes misturados!")
	}
	if sink2.count(engine.EventCompleted) != 1 {
		t.Fatal("esperado 1 evento Completed")
	}
}

func atomicLoad(p *int64) int64 { return atomic.LoadInt64(p) }

// --- Teste 2: servidor SEM Range — recusa retoma honesta, sem corrupção ---

func TestNoRangeServerSafeRestart(t *testing.T) {
	payload := []byte(strings.Repeat("X", 120*1024))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ignora Range sempre: responde 200 corpo inteiro
		w.Header().Set("Accept-Ranges", "none")
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	resumes := map[string]*ResumeState{}
	var rmu sync.Mutex
	e := New(Options{
		ProgressEvery: 5 * time.Millisecond,
		ResumeProvider: func(id string) (*ResumeState, bool) {
			rmu.Lock()
			defer rmu.Unlock()
			rs, ok := resumes[id]
			return rs, ok
		},
		ResumePersist: func(rs ResumeState) { rmu.Lock(); defer rmu.Unlock(); cp := rs; resumes[rs.TaskID] = &cp },
	})
	defer e.Close()

	task := mkTask(srv.URL+"/x.bin", dir)
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: &recSink{}})
	if err != nil {
		t.Fatal(err)
	}
	j := h.(*job)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomicLoad(&j.bytesDone) > 20*1024 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.Pause(context.Background())
	<-j.done
	partial := atomicLoad(&j.bytesDone)
	if partial == 0 {
		t.Skip("transferência demasiado rápida para pausar a meio")
	}

	// retomar contra servidor sem Range => deve RECOMEÇAR do zero (200),
	// nunca aceitar o corpo completo como continuação do offset
	runToCompletion(t, e, task, &recSink{})
	data, err := os.ReadFile(filepath.Join(dir, "x.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Fatalf("conteúdo corrompido: %d vs %d bytes", len(data), len(payload))
	}
}

// --- Teste 3: URL expirada (403) => diagnóstico ErrURLExpired ---

func TestExpiredURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	e := New(Options{})
	defer e.Close()
	sink := &recSink{}
	task := mkTask(srv.URL+"/gone?token=abc", t.TempDir())
	h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	<-h.(*job).done
	de := sink.lastErr()
	if de == nil || de.Class != domain.ErrURLExpired {
		t.Fatalf("esperado ErrURLExpired, obtido %#v", de)
	}
	if !strings.Contains(de.Message, "expirado") {
		t.Fatalf("mensagem pouco útil: %s", de.Message)
	}
}

// --- Teste 4: origem PÚBLICA a redirecionar para loopback é BLOQUEADA (SSRF) ---

func TestRedirectToLoopbackBlocked(t *testing.T) {
	var innerSrv *httptest.Server
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, innerSrv.URL+"/secret", http.StatusFound)
	}))
	innerSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("SECRET-METADATA"))
	}))
	defer outer.Close()
	defer innerSrv.Close()

	// Origem PÚBLICA (IP público literal) cujo tráfego é desviado para o
	// servidor de teste: só assim o redirecionamento público → local é real.
	outerAddr := strings.TrimPrefix(outer.URL, "http://")
	pubClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "93.184.216.34") { // só o host "público" é desviado
				addr = outerAddr
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	e := New(Options{Client: pubClient})
	defer e.Close()
	sink := &recSink{}
	task := mkTask("http://93.184.216.34/start", t.TempDir())
	h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	<-h.(*job).done
	de := sink.lastErr()
	if de == nil {
		t.Fatal("redirecionamento de origem pública para serviço local deveria falhar")
	}
	// o erro de redirect bloqueado chega como connection-level; verificar que
	// o segredo NUNCA foi escrito em disco
	files, _ := os.ReadDir(task.Policy.DestinationDir)
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(task.Policy.DestinationDir, f.Name()))
		if strings.Contains(string(b), "SECRET-METADATA") {
			t.Fatal("SSRF: conteúdo de serviço local foi descarregado!")
		}
	}
}

// --- Teste 5: SHA-256 exigido pela política — mismatch impede conclusão ---

func TestIntegrityMismatchBlocksComplete(t *testing.T) {
	payload := []byte("conteudo real")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	e := New(Options{})
	defer e.Close()
	sink := &recSink{}
	task := mkTask(srv.URL+"/a.txt", t.TempDir())
	sum := sha256.Sum256([]byte("hash diferente"))
	task.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: hex.EncodeToString(sum[:])}}
	task.Policy.RequireIntegrity = domain.IntegritySHA256
	h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	<-h.(*job).done
	if h.(*job).err == nil {
		t.Fatal("deveria falhar por integridade")
	}
	if sink.count(engine.EventCompleted) != 0 {
		t.Fatal("Completed emitido apesar de hash errado")
	}
	if sink.count(engine.EventIntegrityReport) != 1 {
		t.Fatal("faltou relatório de integridade")
	}
}

func TestIntegrityMatchCompletes(t *testing.T) {
	payload := []byte("conteudo bom")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	e := New(Options{})
	defer e.Close()
	sink := &recSink{}
	task := mkTask(srv.URL+"/b.txt", t.TempDir())
	sum := sha256.Sum256(payload)
	task.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: hex.EncodeToString(sum[:])}}
	task.Policy.RequireIntegrity = domain.IntegritySHA256
	runToCompletion(t, e, task, sink)
	if sink.count(engine.EventCompleted) != 1 {
		t.Fatal("esperado Completed com hash correto")
	}
}

// --- Teste 6: Content-Range mentiroso => recusa segura ---

func TestLyingContentRangeRejected(t *testing.T) {
	payload := make([]byte, 100*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 999-1099/102400") // mente!
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[:100])
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	resumes := map[string]*ResumeState{}
	var rmu sync.Mutex
	e := New(Options{
		ProgressEvery: 5 * time.Millisecond,
		ResumeProvider: func(id string) (*ResumeState, bool) {
			rmu.Lock()
			defer rmu.Unlock()
			rs, ok := resumes[id]
			return rs, ok
		},
		ResumePersist: func(rs ResumeState) { rmu.Lock(); defer rmu.Unlock(); cp := rs; resumes[rs.TaskID] = &cp },
	})
	defer e.Close()

	task := mkTask(srv.URL+"/lie.bin", dir)
	// Pausa deterministica apos escrita de pelo menos um chunk: o hook é
	// armado ANTES do Start (armar depois é uma corrida com a goroutine).
	testSetPauseHook(task.ID)
	h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: &recSink{}})
	j := h.(*job)
	<-j.done
	if atomicLoad(&j.bytesDone) == 0 {
		t.Fatal("pausa deveria ocorrer apos escrita de um chunk")
	}
	// A nova corrida deve pausar ANTES de qualquer escrita (estado de retoma
	// valido ja persistido) - assim a resposta mentirosa e exercida na retoma.
	testSetPauseHook(task.ID)
	sink2 := &recSink{}
	h2, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink2})
	if err != nil {
		t.Fatal(err)
	}
	<-h2.(*job).done
	de := sink2.lastErr()
	if de == nil || de.Class != domain.ErrResumeMismatch {
		t.Fatalf("Content-Range falso deveria causar ErrResumeMismatch, obtido %#v", de)
	}
}

// --- Teste 7: limite de velocidade aproximado ---

func TestSpeedLimit(t *testing.T) {
	payload := make([]byte, 200*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	e := New(Options{})
	defer e.Close()
	task := mkTask(srv.URL+"/slow.bin", t.TempDir())
	task.Policy.SpeedLimitBps = 50 * 1024 // 50 KiB/s => ~4s p/ 200 KiB
	start := time.Now()
	runToCompletion(t, e, task, &recSink{})
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("limite de velocidade ignorado: %v para 200KiB@50KiB/s", elapsed)
	}
}

// --- Teste 8: MaxSizeBytes excedido => policy violation antes de escrever ---

func TestMaxSizePolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		// não escrever o corpo todo; encerrar cedo também é testável via EOF
	}))
	defer srv.Close()
	e := New(Options{})
	defer e.Close()
	sink := &recSink{}
	task := mkTask(srv.URL+"/big", t.TempDir())
	task.Policy.MaxSizeBytes = 1000
	h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	<-h.(*job).done
	de := sink.lastErr()
	if de == nil || de.Class != domain.ErrPolicyViolation {
		t.Fatalf("esperado ErrPolicyViolation, obtido %#v", de)
	}
}

// Pausar e retomar de imediato (como faz o orquestrador) não pode falhar com
// "tarefa já ativa" enquanto a goroutine pausada ainda está a terminar.
func TestStartAfterPauseWaitsForPreviousJob(t *testing.T) {
	payload := make([]byte, 200*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "p.bin", time.Unix(0, 0), strings.NewReader(string(payload)))
	}))
	defer srv.Close()

	resumes := map[string]*ResumeState{}
	var rmu sync.Mutex
	e := New(Options{
		ResumeProvider: func(id string) (*ResumeState, bool) {
			rmu.Lock()
			defer rmu.Unlock()
			rs, ok := resumes[id]
			return rs, ok
		},
		ResumePersist: func(rs ResumeState) { rmu.Lock(); defer rmu.Unlock(); cp := rs; resumes[rs.TaskID] = &cp },
	})
	defer e.Close()

	task := mkTask(srv.URL+"/p.bin", t.TempDir())
	task.Policy.SpeedLimitBps = 100 * 1024 // job longa (~2s): ainda viva ao pausar
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: &recSink{}})
	if err != nil {
		t.Fatal(err)
	}
	// Esperar progresso real antes de pausar (retoma só persiste com bytes).
	deadline := time.Now().Add(5 * time.Second)
	for atomicLoad(&h.(*job).bytesDone) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sem progresso")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Pausa explícita imediatamente seguida de novo Start, sem esperar por done.
	_ = h.Pause(context.Background())
	task.Policy.SpeedLimitBps = 0
	sink := &recSink{}
	h2, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: sink})
	if err != nil {
		t.Fatalf("Start logo após pausa deveria aguardar a job anterior: %v", err)
	}
	select {
	case <-h2.(*job).done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
	if h2.(*job).err != nil {
		t.Fatalf("retoma falhou: %v", h2.(*job).err)
	}
	if sink.count(engine.EventCompleted) != 1 {
		t.Fatal("esperado Completed após retoma")
	}
}

func mustReq(t *testing.T, u string) *http.Request {
	r, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A regra anti-SSRF que importa: público → privado é bloqueado; dentro da rede
// privada (origem escolhida pelo utilizador) ou público → público, passa.
func TestRedirectPolicyPublicToPrivateBlockedPrivateToPrivateAllowed(t *testing.T) {
	cases := []struct {
		name      string
		from, to  string
		wantBlock bool
	}{
		{"público → loopback", "http://93.184.216.34/a", "http://127.0.0.1/admin", true},
		{"público → rede interna", "http://93.184.216.34/a", "http://192.168.0.1/router", true},
		{"público → público", "http://93.184.216.34/a", "https://1.1.1.1/b", false},
		{"NAS → NAS", "http://192.168.1.5/a", "http://192.168.1.5/b", false},
		{"localhost → localhost", "http://127.0.0.1:9000/a", "http://127.0.0.1:9000/b", false},
		{"NAS → público (CDN)", "http://192.168.1.5/a", "https://1.1.1.1/b", false},
	}
	for _, c := range cases {
		err := CheckRedirect([]*http.Request{mustReq(t, c.from)}, mustReq(t, c.to))
		if (err != nil) != c.wantBlock {
			t.Errorf("%s: bloqueado=%v (esperado %v): %v", c.name, err != nil, c.wantBlock, err)
		}
	}
}

// Descarregar de um servidor local que redireciona dentro da própria máquina funciona.
func TestLocalServerRedirectDownloads(t *testing.T) {
	data := make([]byte, 20_000)
	srv := httptest.NewServer(nil)
	defer srv.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/new.bin", http.StatusFound) })
	mux.HandleFunc("/new.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "new.bin", time.Unix(0, 0), strings.NewReader(string(data)))
	})
	srv.Config.Handler = mux
	e := New(Options{})
	defer e.Close()
	task := mkTask(srv.URL+"/old", t.TempDir())
	s := &recSink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s})
	if err != nil {
		t.Fatal(err)
	}
	<-h.(*job).done
	if h.(*job).err != nil || s.count(engine.EventCompleted) != 1 {
		t.Fatalf("redirecionamento local deveria funcionar: %v", h.(*job).err)
	}
}

// O Referer/User-Agent do navegador chegam ao servidor; valores inválidos nunca
// viram injeção de cabeçalhos.
func TestBrowserHintsAppliedAndSanitized(t *testing.T) {
	var mu sync.Mutex
	var gotRef, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotRef, gotUA = r.Header.Get("Referer"), r.Header.Get("User-Agent")
		mu.Unlock()
		http.ServeContent(w, r, "h.bin", time.Unix(0, 0), strings.NewReader("conteudo"))
	}))
	defer srv.Close()
	run := func(md map[string]string) {
		e := New(Options{})
		defer e.Close()
		task := mkTask(srv.URL+"/h.bin", t.TempDir())
		task.Source.Metadata = md
		s := &recSink{}
		h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s})
		<-h.(*job).done
		if h.(*job).err != nil {
			t.Fatalf("falhou: %v", h.(*job).err)
		}
	}
	run(map[string]string{"referer": "https://site.example.com/pagina?id=1#frag", "user_agent": "Mozilla/5.0 Teste"})
	mu.Lock()
	if gotRef != "https://site.example.com/pagina?id=1" || gotUA != "Mozilla/5.0 Teste" {
		t.Errorf("pistas não aplicadas: referer=%q ua=%q", gotRef, gotUA)
	}
	mu.Unlock()
	// inválidos: ignorados (nada de CRLF, esquemas estranhos ou credenciais)
	run(map[string]string{"referer": "javascript:alert(1)", "user_agent": "x\r\nX-Evil: 1"})
	mu.Lock()
	defer mu.Unlock()
	if gotRef != "" || strings.Contains(gotUA, "X-Evil") || !strings.HasPrefix(gotUA, "StreamVaultCore/") {
		t.Errorf("valores inválidos deviam ser ignorados: referer=%q ua=%q", gotRef, gotUA)
	}
}

// A pasta de destino (mesmo aninhada) é criada; se for impossível, o erro é explicado.
func TestDestinationFolderIsCreatedOrErrorIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "d.bin", time.Unix(0, 0), strings.NewReader("conteudo"))
	}))
	defer srv.Close()
	run := func(dest string) *recSink {
		e := New(Options{})
		defer e.Close()
		task := mkTask(srv.URL+"/d.bin", dest)
		s := &recSink{}
		h, _ := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s})
		<-h.(*job).done
		return s
	}
	base := t.TempDir()
	nested := filepath.Join(base, "a", "b", "c")
	if s := run(nested); s.count(engine.EventCompleted) != 1 {
		t.Fatalf("pasta aninhada devia ser criada: %+v", s.lastErr())
	}
	if _, err := os.Stat(filepath.Join(nested, "d.bin")); err != nil {
		t.Fatalf("ficheiro não está no destino: %v", err)
	}
	// o destino "pasta" é na verdade um ficheiro: erro classificado, não "unknown"
	file := filepath.Join(base, "ficheiro")
	os.WriteFile(file, []byte("x"), 0o644)
	s := run(filepath.Join(file, "sub"))
	de := s.lastErr()
	if de == nil || de.Class == domain.ErrUnknown || de.Action == "" {
		t.Fatalf("erro de destino inválido devia ser classificado: %+v", de)
	}
}
