package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const apiCookie = "sid=segredo-api-123"

// protectedServer exige o cookie e regista o Cookie visto em cada pedido.
func protectedServer(t *testing.T, size int, requireCookie bool) (*httptest.Server, func() []string) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Cookie"))
		mu.Unlock()
		if requireCookie && r.Header.Get("Cookie") != apiCookie {
			http.Error(w, "login", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func TestNewPostRoutesRequireToken(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/v1/capture/links", "/v1/tasks/batch"} {
		if c, _, _ := e.do("POST", p, map[string]any{"url": "http://x"}, "", nil); c != 401 {
			t.Errorf("%s sem token: %d (esperado 401)", p, c)
		}
	}
}

func TestAddTaskWithCookieDownloadsAndNeverEchoesIt(t *testing.T) {
	e := newEnv(t, nil)
	srv, _ := protectedServer(t, 40_000, true)

	code, m, raw := e.do("POST", "/v1/tasks", map[string]string{"url": srv.URL + "/privado.bin", "cookie": apiCookie}, tok, nil)
	if code != 201 {
		t.Fatalf("POST com cookie: %d %s", code, raw)
	}
	id := m["id"].(string)
	e.waitState(id, "completed")

	_, _, all := e.do("GET", "/v1/tasks", nil, tok, nil)
	for _, b := range [][]byte{raw, all} {
		if strings.Contains(string(b), "segredo-api-123") {
			t.Fatalf("o cookie foi devolvido pela API: %s", b)
		}
	}
	// cookie inválido => 4xx explicado, sem tarefa criada
	before := len(e.tasks())
	code, _, raw = e.do("POST", "/v1/tasks", map[string]string{"url": srv.URL + "/x.bin", "cookie": "a=b\r\nEvil: 1"}, tok, nil)
	if code < 400 || code >= 500 || len(e.tasks()) != before {
		t.Fatalf("cookie inválido devia ser recusado sem criar tarefa: %d %s", code, raw)
	}
	if strings.Contains(string(raw), "Evil") {
		t.Errorf("a resposta de erro não deve repetir o cookie: %s", raw)
	}
}

func TestCaptureLinksEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/vazia" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><a href="/sobre">sobre</a></html>`)
			return
		}
		if r.Header.Get("Cookie") != apiCookie {
			http.Error(w, "login", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/lista":
			fmt.Fprint(w, `<a href="/m/manual.pdf">Manual</a><a href="/m/tudo.zip">Tudo</a><a rel="next" href="/lista2">mais</a>`)
		case "/lista2":
			fmt.Fprint(w, `<a href="/m/extra.pdf">Extra</a>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer page.Close()

	// atrás de login, com filtro de categoria e paginação
	code, m, raw := e.do("POST", "/v1/capture/links", map[string]any{
		"url": page.URL + "/lista", "cookie": apiCookie, "categories": []string{"Documentos"}, "max_pages": 3}, tok, nil)
	if code != 200 {
		t.Fatalf("capture/links: %d %s", code, raw)
	}
	links := m["links"].([]any)
	if len(links) != 2 { // manual.pdf + extra.pdf; o zip fica de fora pelo filtro
		t.Fatalf("esperava 2 PDFs (2 páginas), obtido %d: %s", len(links), raw)
	}
	if pages := m["pages"].([]any); len(pages) != 2 {
		t.Errorf("páginas lidas: %v", pages)
	}
	if strings.Contains(string(raw), "segredo-api-123") {
		t.Fatal("o cookie foi devolvido pela API")
	}
	// sem cookie a página protegida é recusada com erro claro
	if code, _, _ := e.do("POST", "/v1/capture/links", map[string]any{"url": page.URL + "/lista"}, tok, nil); code != 422 {
		t.Errorf("sem cookie esperava 422, obtido %d", code)
	}
	// página sem ficheiros: lista vazia (não null)
	code, m, raw = e.do("POST", "/v1/capture/links", map[string]any{"url": page.URL + "/vazia"}, tok, nil)
	if code != 200 || m["links"] == nil || len(m["links"].([]any)) != 0 {
		t.Errorf("página sem ficheiros: %d %s", code, raw)
	}
	// validação
	for name, body := range map[string]any{
		"url vazia":         map[string]any{"url": ""},
		"campo inexistente": map[string]any{"url": page.URL, "nada": 1},
	} {
		if code, _, _ := e.do("POST", "/v1/capture/links", body, tok, nil); code != 400 {
			t.Errorf("%s: esperava 400, obtido %d", name, code)
		}
	}
}

func TestBatchAddScopesCookieToTheSessionHost(t *testing.T) {
	e := newEnv(t, nil)
	site, siteSeen := protectedServer(t, 30_000, true)    // onde a sessão foi iniciada
	other, otherSeen := protectedServer(t, 30_000, false) // outro host: público

	code, m, raw := e.do("POST", "/v1/tasks/batch", map[string]any{
		"urls":       []string{site.URL + "/a.bin?token=SEGREDO-URL", other.URL + "/b.bin", "", "nao-e-url"},
		"cookie":     apiCookie,
		"cookie_url": site.URL + "/pagina",
	}, tok, nil)
	if code != 200 {
		t.Fatalf("batch: %d %s", code, raw)
	}
	res := m["results"].([]any)
	if len(res) != 4 {
		t.Fatalf("um resultado por ligação: %s", raw)
	}
	r0, r1, r2, r3 := res[0].(map[string]any), res[1].(map[string]any), res[2].(map[string]any), res[3].(map[string]any)
	if r0["ok"] != true || r0["cookie_applied"] != true {
		t.Errorf("a ligação do site da sessão leva o cookie: %v", r0)
	}
	if r1["ok"] != true || r1["cookie_applied"] == true {
		t.Errorf("a ligação de outro host NÃO leva o cookie: %v", r1)
	}
	if r2["ok"] == true || r2["error"] == "" || r3["ok"] == true || r3["error"] == "" {
		t.Errorf("ligações inválidas falham individualmente, sem parar o lote: %v %v", r2, r3)
	}
	e.waitState(r0["id"].(string), "completed")
	e.waitState(r1["id"].(string), "completed")
	for _, c := range otherSeen() {
		if c != "" {
			t.Fatalf("o cookie vazou para o outro host: %q", c)
		}
	}
	if len(siteSeen()) == 0 {
		t.Fatal("o site da sessão devia ter sido contactado")
	}
	if strings.Contains(string(raw), "SEGREDO-URL") || strings.Contains(string(raw), "segredo-api-123") {
		t.Fatalf("o lote expôs a query do URL ou o cookie: %s", raw)
	}

	// validações do pedido
	if code, _, _ := e.do("POST", "/v1/tasks/batch", map[string]any{"urls": []string{site.URL + "/a.bin"}, "cookie": apiCookie}, tok, nil); code != 400 {
		t.Errorf("cookie sem cookie_url: esperava 400, obtido %d", code)
	}
	if code, _, _ := e.do("POST", "/v1/tasks/batch", map[string]any{"urls": []string{}}, tok, nil); code != 400 {
		t.Errorf("lote vazio: esperava 400, obtido %d", code)
	}
	many := make([]string, 201)
	for i := range many {
		many[i] = site.URL + "/x.bin"
	}
	if code, _, _ := e.do("POST", "/v1/tasks/batch", map[string]any{"urls": many}, tok, nil); code != 400 {
		t.Errorf("lote acima do limite: esperava 400, obtido %d", code)
	}
}
