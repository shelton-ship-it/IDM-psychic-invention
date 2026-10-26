package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"streamvault/core/app"
	"streamvault/core/config"
)

const tok = "0123456789abcdef-token"

type env struct {
	t    *testing.T
	core *app.Core
	srv  *httptest.Server
	dl   string
	lib  string
}

func newEnv(t *testing.T, mod func(*config.Config)) *env {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DefaultDir = t.TempDir()
	cfg.Storage.LibraryDir = t.TempDir()
	cfg.Security.AllowPrivateNetworks = true // origens de teste são loopback
	if mod != nil {
		mod(&cfg)
	}
	core, err := app.New(cfg, app.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(core, tok)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); core.Close() })
	return &env{t: t, core: core, srv: srv, dl: cfg.Storage.DefaultDir, lib: cfg.Storage.LibraryDir}
}

func (e *env) do(method, path string, body any, token string, hdr map[string]string) (int, map[string]any, []byte) {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	raw.ReadFrom(resp.Body)
	var m map[string]any
	json.Unmarshal(raw.Bytes(), &m)
	return resp.StatusCode, m, raw.Bytes()
}

func (e *env) tasks() []map[string]any {
	e.t.Helper()
	_, _, raw := e.do("GET", "/v1/tasks", nil, tok, nil)
	var l []map[string]any
	json.Unmarshal(raw, &l)
	return l
}

func (e *env) waitState(id, want string) map[string]any {
	e.t.Helper()
	for i := 0; i < 1500; i++ {
		for _, t := range e.tasks() {
			if t["id"] == id && t["state"] == want {
				return t
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timeout à espera de %s (%v)", want, e.tasks())
	return nil
}

func fileServer(t *testing.T, size int, slow bool) *httptest.Server {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if slow {
			time.Sleep(0) // ServeContent respeita o limite de velocidade do motor
		}
		http.ServeContent(w, r, "f.bin", time.Unix(0, 0), bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAuthRequiredOnAllV1Routes(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/v1/health", "/v1/tasks", "/v1/plugins", "/v1/config", "/v1/events"} {
		if c, _, _ := e.do("GET", p, nil, "", nil); c != 401 {
			t.Errorf("%s sem token: %d (esperado 401)", p, c)
		}
		if c, _, _ := e.do("GET", p, nil, "token-errado-123456", nil); c != 401 {
			t.Errorf("%s token errado: %d (esperado 401)", p, c)
		}
	}
	if c, m, _ := e.do("GET", "/v1/health", nil, tok, nil); c != 200 || m["ok"] != true {
		t.Fatalf("health com token: %d %v", c, m)
	}
	if _, err := New(e.core, "curto"); err == nil {
		t.Error("token curto deve ser recusado")
	}
}

func TestRejectsForeignHostAndOrigin(t *testing.T) {
	e := newEnv(t, nil)
	if c, _, _ := e.do("GET", "/v1/health", nil, tok, map[string]string{"Host": "evil.example.com"}); c != 403 {
		t.Errorf("Host estrangeiro (DNS rebinding): %d (esperado 403)", c)
	}
	if c, _, _ := e.do("POST", "/v1/tasks", map[string]string{"url": "http://x/a"}, tok, map[string]string{"Origin": "https://evil.example.com"}); c != 403 {
		t.Errorf("Origin estrangeira (CSRF): %d (esperado 403)", c)
	}
	// a própria origem passa
	own := e.srv.URL
	if c, _, _ := e.do("GET", "/v1/health", nil, tok, map[string]string{"Origin": own}); c != 200 {
		t.Errorf("origem própria bloqueada: %d", c)
	}
}

func TestAddTaskDownloadsAndNeverLeaksQuery(t *testing.T) {
	e := newEnv(t, nil)
	fs := fileServer(t, 50_000, false)
	code, m, raw := e.do("POST", "/v1/tasks", map[string]string{"url": fs.URL + "/f.bin?token=SEGREDO123"}, tok, nil)
	if code != 201 {
		t.Fatalf("POST: %d %s", code, raw)
	}
	id := m["id"].(string)
	done := e.waitState(id, "completed")
	if _, err := os.Stat(filepath.Join(e.dl, "f.bin")); err != nil {
		t.Fatalf("ficheiro não chegou ao destino: %v", err)
	}
	_, _, all := e.do("GET", "/v1/tasks", nil, tok, nil)
	if strings.Contains(string(all), "SEGREDO123") || strings.Contains(string(raw), "SEGREDO123") {
		t.Fatal("a query/token da URL foi exposta pela API")
	}
	if done["name"] != "f.bin" {
		t.Errorf("nome final: %v", done["name"])
	}
}

func TestRequestValidation(t *testing.T) {
	e := newEnv(t, nil)
	if c, _, _ := e.do("POST", "/v1/tasks", map[string]string{"url": "  "}, tok, nil); c != 400 {
		t.Errorf("url vazia: %d", c)
	}
	if c, _, _ := e.do("POST", "/v1/tasks", map[string]any{"url": "http://x/a", "dir": "/etc"}, tok, nil); c != 400 {
		t.Errorf("campo desconhecido (dir) deve ser recusado: %d", c)
	}
	if c, _, _ := e.do("POST", "/v1/tasks", map[string]string{"url": "isto não é um link"}, tok, nil); c != 422 {
		t.Errorf("link não reconhecido: %d", c)
	}
	if c, _, _ := e.do("GET", "/v1/nada", nil, tok, nil); c != 404 {
		t.Errorf("rota inexistente: %d", c)
	}
	if c, _, _ := e.do("DELETE", "/v1/tasks/inexistente", nil, tok, nil); c != 404 {
		t.Errorf("remover inexistente: %d", c)
	}
}

func TestPrivateNetworkBlockedWith403InStrictMode(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Security.AllowPrivateNetworks = false })
	code, m, _ := e.do("POST", "/v1/tasks", map[string]string{"url": "http://127.0.0.1:9/x.bin"}, tok, nil)
	if code != 403 {
		t.Fatalf("esperava 403, obtido %d", code)
	}
	if em, _ := m["error"].(map[string]any); em["class"] != "policy_violation" || em["action"] == "" {
		t.Errorf("erro sem diagnóstico: %v", m)
	}
	if len(e.tasks()) != 0 {
		t.Error("origem bloqueada entrou na fila")
	}
}

func TestPauseResumeCancelDeleteLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	fs := fileServer(t, 600_000, true)
	// limite de velocidade por tarefa para a manter viva durante o teste
	e.core.Cfg.Bandwidth.PerTaskLimitBps = 150 * 1024
	_, m, _ := e.do("POST", "/v1/tasks", map[string]string{"url": fs.URL + "/f.bin"}, tok, nil)
	id := m["id"].(string)
	part := filepath.Join(e.dl, "f.bin.svpart")
	for i := 0; ; i++ {
		if fi, err := os.Stat(part); err == nil && fi.Size() > 0 {
			break
		}
		if i > 500 {
			t.Fatal("download não avançou")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c, _, _ := e.do("DELETE", "/v1/tasks/"+id, nil, tok, nil); c != 409 {
		t.Errorf("remover tarefa ativa deve dar 409, deu %d", c)
	}
	if c, _, raw := e.do("POST", "/v1/tasks/"+id+"/pause", nil, tok, nil); c != 200 {
		t.Fatalf("pause: %d %s", c, raw)
	}
	e.waitState(id, "paused")
	if c, _, raw := e.do("POST", "/v1/tasks/"+id+"/resume", nil, tok, nil); c != 200 {
		t.Fatalf("resume: %d %s", c, raw)
	}
	if c, _, raw := e.do("POST", "/v1/tasks/"+id+"/cancel", nil, tok, nil); c != 200 {
		t.Fatalf("cancel: %d %s", c, raw)
	}
	e.waitState(id, "cancelled")
	if c, _, _ := e.do("DELETE", "/v1/tasks/"+id, nil, tok, nil); c != 204 {
		t.Errorf("remover cancelada: %d", c)
	}
	if len(e.tasks()) != 0 {
		t.Error("tarefa não foi removida")
	}
}

func TestOrganizeThroughAPIUsesConfiguredLibrary(t *testing.T) {
	e := newEnv(t, nil)
	fs := fileServer(t, 20_000, false)
	_, m, _ := e.do("POST", "/v1/tasks", map[string]string{"url": fs.URL + "/clip.mp4"}, tok, nil)
	id := m["id"].(string)
	e.waitState(id, "completed")
	code, r, raw := e.do("POST", "/v1/tasks/"+id+"/organize", map[string]any{}, tok, nil)
	if code != 200 || r["action"] != "moved" || r["category"] != "Videos" {
		t.Fatalf("organize: %d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(e.lib, "Videos", "clip.mp4")); err != nil {
		t.Fatalf("não está na biblioteca configurada: %v", err)
	}
	// o cliente NÃO pode escolher a pasta: campo desconhecido é recusado
	if c, _, _ := e.do("POST", "/v1/tasks/"+id+"/organize", map[string]any{"base_dir": "/tmp"}, tok, nil); c != 400 {
		t.Errorf("base_dir vindo do cliente deve ser recusado: %d", c)
	}
}

func TestEventsStreamDeliversCompletion(t *testing.T) {
	e := newEnv(t, nil)
	fs := fileServer(t, 30_000, false)
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: %s", ct)
	}
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") && strings.Contains(line, `"kind":"completed"`) {
				got <- line
				return
			}
		}
	}()
	e.do("POST", "/v1/tasks", map[string]string{"url": fs.URL + "/f.bin"}, tok, nil)
	select {
	case line := <-got:
		var ev map[string]any
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
		if ev["task_id"] == "" {
			t.Errorf("evento sem task_id: %s", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("evento completed não chegou por SSE")
	}
}

func TestUIServedWithStrictCSPAndNoInnerHTML(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(b.String(), "StreamVault") {
		t.Fatalf("UI: %d", resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "http:") {
		t.Errorf("CSP fraca: %q", csp)
	}
	// nomes de ficheiros vêm da rede: a UI só pode usar textContent
	if strings.Contains(b.String(), "innerHTML") {
		t.Error("a UI não pode usar innerHTML (XSS com nomes remotos)")
	}
	if c, _, _ := e.do("GET", "/nao-existe", nil, "", nil); c != 404 {
		t.Errorf("rota desconhecida: %d", c)
	}
}

func TestValidateListenAddr(t *testing.T) {
	ok := []string{"127.0.0.1:0", "127.0.0.1:8080", "[::1]:9", "localhost:7000"}
	bad := []string{"0.0.0.0:8080", ":8080", "192.168.1.5:80", "example.com:80", "sem-porta"}
	for _, a := range ok {
		if err := ValidateListenAddr(a); err != nil {
			t.Errorf("%s deveria ser aceite: %v", a, err)
		}
	}
	for _, a := range bad {
		if err := ValidateListenAddr(a); err == nil {
			t.Errorf("%s deveria ser recusado", a)
		}
	}
}

// Fluxo completo do IDM: página → candidatos → download com o Referer da página
// (o servidor de mídia recusa hotlinking sem ele).
func TestCaptureThenDownloadWithRefererThroughAPI(t *testing.T) {
	e := newEnv(t, nil)
	data := bytes.Repeat([]byte("v"), 40_000)
	var site *httptest.Server
	var mu sync.Mutex
	var pageUA string
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aula":
			mu.Lock()
			pageUA = r.Header.Get("User-Agent")
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<title>Aula 3</title><video src="/media/aula3.mp4?sig=SEGREDO"></video><script>x={"u":"` +
				`/nao-e-midia"}</script>`))
		case "/media/aula3.mp4":
			if r.Header.Get("Referer") != site.URL+"/aula" { // anti-hotlinking
				http.Error(w, "proibido", http.StatusForbidden)
				return
			}
			http.ServeContent(w, r, "aula3.mp4", time.Unix(0, 0), bytes.NewReader(data))
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()

	code, m, raw := e.do("POST", "/v1/capture", map[string]string{"url": site.URL + "/aula", "user_agent": "Mozilla/5.0 Nav"}, tok, nil)
	if code != 200 {
		t.Fatalf("capture: %d %s", code, raw)
	}
	cands := m["candidates"].([]any)
	if len(cands) != 1 || m["title"] != "Aula 3" {
		t.Fatalf("candidatos: %s", raw)
	}
	c := cands[0].(map[string]any)
	if c["kind"] != "video" || c["supported"] != true {
		t.Fatalf("candidato: %v", c)
	}
	mu.Lock()
	if pageUA != "Mozilla/5.0 Nav" {
		t.Errorf("a captura deve usar o User-Agent do navegador: %q", pageUA)
	}
	mu.Unlock()

	// sem Referer o servidor recusa
	code, m, _ = e.do("POST", "/v1/tasks", map[string]string{"url": c["url"].(string)}, tok, nil)
	if code != 201 {
		t.Fatalf("add sem referer: %d", code)
	}
	failed := e.waitState(m["id"].(string), "failed")
	if failed["error"] == nil {
		t.Error("falha sem diagnóstico")
	}
	// com o Referer da página, descarrega
	code, m, raw = e.do("POST", "/v1/tasks", map[string]string{"url": c["url"].(string), "referer": site.URL + "/aula", "user_agent": "Mozilla/5.0 Nav"}, tok, nil)
	if code != 201 {
		t.Fatalf("add com referer: %d %s", code, raw)
	}
	e.waitState(m["id"].(string), "completed")
	// o Referer (e a query assinada) nunca aparecem na API
	_, _, all := e.do("GET", "/v1/tasks", nil, tok, nil)
	if strings.Contains(string(all), "SEGREDO") || strings.Contains(string(all), site.URL+"/aula\"") || strings.Contains(string(all), "referer") {
		t.Fatalf("a API expôs referer/query: %s", all)
	}
}

func TestCaptureValidationAndStrictMode(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Security.AllowPrivateNetworks = false })
	if c, _, _ := e.do("POST", "/v1/capture", map[string]string{"url": " "}, tok, nil); c != 400 {
		t.Errorf("url vazia: %d", c)
	}
	if c, _, _ := e.do("POST", "/v1/capture", map[string]any{"url": "http://x/", "extra": 1}, tok, nil); c != 400 {
		t.Errorf("campo desconhecido: %d", c)
	}
	if c, _, _ := e.do("POST", "/v1/capture", map[string]string{"url": "http://127.0.0.1:9/p"}, tok, nil); c != 422 {
		t.Errorf("página em rede privada no modo estrito: %d", c)
	}
	if c, _, _ := e.do("POST", "/v1/capture", map[string]string{"url": "http://x/"}, "", nil); c != 401 {
		t.Errorf("sem token: %d", c)
	}
}

// Extensões de navegador: aceites (com token); páginas web de outras origens: nunca.
func TestExtensionOriginsAndPreflight(t *testing.T) {
	e := newEnv(t, nil)
	ext := "chrome-extension://abcdefghijklmnopabcdefghijklmnop"
	// pedido real com token
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Origin", ext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != ext {
		t.Fatalf("extensão com token: %d ACAO=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	// sem token: continua 401 (a origem da extensão não dispensa a chave)
	if c, _, _ := e.do("GET", "/v1/tasks", nil, "", map[string]string{"Origin": ext}); c != 401 {
		t.Errorf("extensão sem token: %d", c)
	}
	// pré-voo
	pf, _ := http.NewRequest("OPTIONS", e.srv.URL+"/v1/tasks", nil)
	pf.Header.Set("Origin", ext)
	pf.Header.Set("Access-Control-Request-Method", "POST")
	pr, err := http.DefaultClient.Do(pf)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body.Close()
	if pr.StatusCode != 204 || !strings.Contains(pr.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("pré-voo: %d %v", pr.StatusCode, pr.Header)
	}
	// origens web e formatos forjados: bloqueados
	for _, o := range []string{"https://evil.example.com", "http://127.0.0.1:1", "chrome-extension://", "chrome-extension://x/evil",
		"chrome-extension://a@evil.com", "null", "file://"} {
		if c, _, _ := e.do("GET", "/v1/health", nil, tok, map[string]string{"Origin": o}); c != 403 {
			t.Errorf("Origin %q deveria dar 403, deu %d", o, c)
		}
	}
	// pré-voo de página web: sem cabeçalhos CORS
	pf2, _ := http.NewRequest("OPTIONS", e.srv.URL+"/v1/tasks", nil)
	pf2.Header.Set("Origin", "https://evil.example.com")
	if r2, err := http.DefaultClient.Do(pf2); err == nil {
		r2.Body.Close()
		if r2.StatusCode != 403 || r2.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("pré-voo de página web deve ser recusado: %d", r2.StatusCode)
		}
	}
}
