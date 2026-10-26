package mobile

import (
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
	"streamvault/core/view"
)

type recListener struct {
	mu sync.Mutex
	ev []view.Event
}

func (r *recListener) OnEvent(s string) {
	var e view.Event
	if json.Unmarshal([]byte(s), &e) == nil {
		r.mu.Lock()
		r.ev = append(r.ev, e)
		r.mu.Unlock()
	}
}
func (r *recListener) has(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ev {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func newCore(t *testing.T, l Listener, mod func(*config.Config)) (*Core, string, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DefaultDir = t.TempDir()
	cfg.Storage.LibraryDir = t.TempDir()
	cfg.Security.AllowPrivateNetworks = true
	if mod != nil {
		mod(&cfg)
	}
	c, err := app.New(cfg, app.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	m := wrap(c, l)
	t.Cleanup(func() { m.Close() })
	return m, cfg.Storage.DefaultDir, cfg.Storage.LibraryDir
}

func tasks(t *testing.T, m *Core) []view.Task {
	t.Helper()
	s, err := m.Tasks()
	if err != nil {
		t.Fatal(err)
	}
	var l []view.Task
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		t.Fatalf("JSON inválido: %v (%s)", err, s)
	}
	return l
}

func TestAddEventsOrganizeRemoveLifecycle(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 40_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.mp3", time.Unix(0, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	l := &recListener{}
	m, dl, lib := newCore(t, l, nil)

	js, err := m.Add(srv.URL + "/a.mp3?sig=SEGREDO")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(js, "SEGREDO") {
		t.Fatal("a query da URL não pode sair pela fachada")
	}
	var added view.Task
	json.Unmarshal([]byte(js), &added)
	for i := 0; ; i++ {
		ts := tasks(t, m)
		if len(ts) == 1 && ts[0].State == "completed" {
			break
		}
		if i > 1000 {
			t.Fatalf("não concluiu: %+v", ts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !l.has("completed") {
		t.Error("o Listener não recebeu o evento completed")
	}
	if err := m.Remove(added.ID); err != nil {
		// ainda não organizado, mas terminado: remover é permitido
		t.Fatal(err)
	}
	if len(tasks(t, m)) != 0 {
		t.Error("tarefa não removida")
	}
	_ = dl
	_ = lib
}

func TestOrganizeAndErrorsAreTyped(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 10_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "p.png", time.Unix(0, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	m, _, lib := newCore(t, nil, nil)
	js, _ := m.Add(srv.URL + "/p.png")
	var tk view.Task
	json.Unmarshal([]byte(js), &tk)
	for i := 0; i < 1000; i++ {
		if ts := tasks(t, m); len(ts) == 1 && ts[0].State == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, err := m.Organize(tk.ID, false)
	if err != nil || !strings.Contains(out, `"moved"`) {
		t.Fatalf("organize: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(lib, "Imagens", "p.png")); err != nil {
		t.Fatalf("não está na biblioteca: %v", err)
	}
	if err := m.Remove("inexistente"); err == nil {
		t.Error("remover inexistente deve falhar")
	}
	if _, err := m.Add("isto não é um link"); err == nil {
		t.Error("link inválido deve falhar")
	}
}

func TestPrivateNetworkBlockedInStrictModeAndOpenValidation(t *testing.T) {
	m, _, _ := newCore(t, nil, func(c *config.Config) { c.Security.AllowPrivateNetworks = false })
	if _, err := m.Add("http://192.168.1.10/x.bin"); err == nil {
		t.Fatal("rede privada deve ser bloqueada no modo estrito")
	}
	if len(tasks(t, m)) != 0 {
		t.Error("origem bloqueada entrou na fila")
	}
	if _, err := Open("", "", nil); err == nil {
		t.Error("Open sem caminhos deve falhar")
	}
	if Version() == "" {
		t.Error("versão vazia")
	}
}
