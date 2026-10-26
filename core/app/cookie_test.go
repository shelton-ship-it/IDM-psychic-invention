package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"streamvault/core/domain"
)

const testCookie = "sid=segredo-da-sessao; csrf=xyz"

// cookieLog regista o Cookie recebido por cada pedido a um servidor de teste.
type cookieLog struct {
	mu   sync.Mutex
	seen []string // "caminho|cookie"
}

func (l *cookieLog) add(r *http.Request) {
	l.mu.Lock()
	l.seen = append(l.seen, r.URL.Path+"|"+r.Header.Get("Cookie"))
	l.mu.Unlock()
}

func (l *cookieLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func (l *cookieLog) anyWith(sub string) bool {
	for _, s := range l.all() {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// HTTP atrás de login: sem cookie 403, com cookie descarrega. O cookie nunca é
// persistido (só um token opaco) e é esquecido quando a tarefa conclui.
func TestEndToEndHTTPDownloadBehindLoginUsesCookieAndNeverPersistsIt(t *testing.T) {
	data := payloadOf(120 * 1024)
	var lg cookieLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lg.add(r)
		if r.Header.Get("Cookie") != testCookie {
			http.Error(w, "inicie sessão", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "f.bin", time.Unix(0, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// sem cookie: falha (e não grava a página de erro como se fosse o ficheiro)
	t0, err := c.Add(srv.URL+"/privado.bin", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, st, t0.ID, domain.StateFailed)

	tk, err := c.AddWith(srv.URL+"/privado.bin?token=abc", domain.DownloadPolicy{}, Hints{Cookie: testCookie})
	if err != nil {
		t.Fatal(err)
	}
	done := waitState(t, st, tk.ID, domain.StateCompleted)
	got, err := os.ReadFile(filepath.Join(dir, "privado.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("ficheiro: err=%v len=%d", err, len(got))
	}

	// o que seria persistido (a tarefa inteira) não contém o cookie, só o token e o host
	raw, _ := json.Marshal(done)
	if bytes.Contains(raw, []byte("segredo-da-sessao")) || bytes.Contains(raw, []byte("csrf=xyz")) {
		t.Fatalf("o cookie NÃO pode estar na tarefa persistida: %s", raw)
	}
	ref := done.Source.Metadata["cookie_ref"]
	if ref == "" || done.Source.Metadata["cookie_host"] == "" || done.Source.Metadata["cookie"] != "" {
		t.Fatalf("esperava só cookie_ref e cookie_host na origem: %+v", done.Source.Metadata)
	}
	if strings.Contains(done.Source.Redacted(), "segredo") {
		t.Errorf("a vista pública não pode expor credenciais: %s", done.Source.Redacted())
	}

	// concluída => credenciais esquecidas (a limpeza corre no evento de conclusão)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := c.secrets.get(ref); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("as credenciais deviam ser apagadas quando a tarefa conclui")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// O cookie não acompanha um redirecionamento para outro host.
func TestHTTPCookieDoesNotFollowCrossHostRedirect(t *testing.T) {
	data := payloadOf(50 * 1024)
	var other cookieLog
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.add(r)
		http.ServeContent(w, r, "f.bin", time.Unix(0, 0), bytes.NewReader(data))
	}))
	defer cdn.Close()
	var main cookieLog
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		main.add(r)
		if r.Header.Get("Cookie") != testCookie {
			http.Error(w, "login", http.StatusForbidden)
			return
		}
		http.Redirect(w, r, cdn.URL+"/real.bin", http.StatusFound)
	}))
	defer site.Close()
	st := NewMemStore()
	c, err := New(testCfg(t.TempDir()), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.AddWith(site.URL+"/dl", domain.DownloadPolicy{}, Hints{Cookie: testCookie})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	if !main.anyWith(testCookie) {
		t.Fatal("o site de origem devia receber o cookie")
	}
	if len(other.all()) == 0 {
		t.Fatal("o teste devia ter seguido o redirecionamento")
	}
	if other.anyWith("segredo-da-sessao") {
		t.Fatalf("o cookie vazou para o outro host: %v", other.all())
	}
}

// HLS atrás de login: o cookie vai à playlist e aos segmentos do MESMO host,
// nunca a um segmento servido por outro host (CDN).
func TestEndToEndHLSCookieStaysOnPlaylistHost(t *testing.T) {
	segA, segB, segC := payloadOf(30*1024), payloadOf(20*1024), payloadOf(25*1024)
	segB[0] ^= 0xFF
	var cdn cookieLog
	cdnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdn.add(r)
		w.Write(segB)
	}))
	defer cdnSrv.Close()
	var main cookieLog
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		main.add(r)
		if r.Header.Get("Cookie") != testCookie {
			http.Error(w, "login", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg0.ts\n#EXTINF:4,\n%s/seg1.ts\n#EXTINF:4,\nseg2.ts\n#EXT-X-ENDLIST\n", cdnSrv.URL)
		case "/seg0.ts":
			w.Write(segA)
		case "/seg2.ts":
			w.Write(segC)
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.AddWith(site.URL+"/v.m3u8", domain.DownloadPolicy{}, Hints{Cookie: testCookie})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Source.Kind != domain.SourceHLS {
		t.Fatalf("esperava HLS, obtido %q", tk.Source.Kind)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	got, err := os.ReadFile(filepath.Join(dir, "v.ts"))
	want := bytes.Join([][]byte{segA, segB, segC}, nil)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("conteúdo HLS: err=%v len=%d (esperado %d)", err, len(got), len(want))
	}
	for _, p := range []string{"/v.m3u8|", "/seg0.ts|", "/seg2.ts|"} {
		if !main.anyWith(p + testCookie) {
			t.Errorf("o pedido %s devia levar o cookie; vistos: %v", p, main.all())
		}
	}
	if len(cdn.all()) == 0 {
		t.Fatal("o segmento do CDN devia ter sido pedido")
	}
	if cdn.anyWith("segredo-da-sessao") {
		t.Fatalf("o cookie vazou para o CDN: %v", cdn.all())
	}
}

// Depois de reiniciar as credenciais já não existem: a tarefa falha com
// explicação (auth_required), em vez de tentar sem login.
func TestTaskWithLostCookieFailsExplainedInsteadOfRunningAnonymous(t *testing.T) {
	var lg cookieLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lg.add(r)
		http.ServeContent(w, r, "f.bin", time.Unix(0, 0), bytes.NewReader(payloadOf(10*1024)))
	}))
	defer srv.Close()
	cfg := testCfg(t.TempDir())
	cfg.Queue.AutoStart = false // dá-nos o instante entre "adicionada" e "iniciada"
	st := NewMemStore()
	c, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.AddWith(srv.URL+"/f.bin", domain.DownloadPolicy{}, Hints{Cookie: testCookie})
	if err != nil {
		t.Fatal(err)
	}
	c.secrets.wipe() // equivale a reiniciar o programa
	c.Pump()
	got := waitState(t, st, tk.ID, domain.StateFailed)
	if got.Error == nil || got.Error.Class != domain.ErrAuthRequired || got.Error.Action == "" {
		t.Fatalf("esperava failed/auth_required com ação, obtido %+v", got.Error)
	}
	if n := len(lg.all()); n != 0 {
		t.Errorf("sem credenciais não podia contactar o servidor; pedidos: %v", lg.all())
	}
}

func TestInvalidOrMisplacedCookieIsRefusedBeforeQueueing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	st := NewMemStore()
	c, err := New(testCfg(t.TempDir()), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cases := map[string]struct{ url, cookie string }{
		"injeção de cabeçalho": {srv.URL + "/f.bin", "a=b\r\nX-Evil: 1"},
		"demasiado longo":      {srv.URL + "/f.bin", "a=" + strings.Repeat("x", 9000)},
		"espaços nas pontas":   {srv.URL + "/f.bin", " a=b "},
		"magnet com cookie":    {"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "a=b"},
	}
	for name, tc := range cases {
		_, err := c.AddWith(tc.url, domain.DownloadPolicy{}, Hints{Cookie: tc.cookie})
		de, ok := err.(*domain.DownloadError)
		if !ok || de.Class != domain.ErrInvalidInput {
			t.Errorf("%s: esperava invalid_input, obtido %#v", name, err)
		}
	}
	if list, _ := st.List(); len(list) != 0 {
		t.Errorf("nada devia ter sido enfileirado: %d tarefas", len(list))
	}
	c.secrets.mu.Lock()
	n := len(c.secrets.m)
	c.secrets.mu.Unlock()
	if n != 0 {
		t.Errorf("pedidos recusados não deixam credenciais em memória: %d", n)
	}
}
