// Package mobile é a fachada para Android (e iOS) via gomobile (ADR-003).
//
// Restrições do gomobile respeitadas: só tipos básicos (string, int64, bool,
// []byte), erros como segundo retorno e callbacks por interface com métodos de
// tipos básicos. Os objetos complexos viajam como JSON (pacote view) — o mesmo
// formato da API do Windows, para as duas UIs partilharem lógica de ecrã.
//
//	gomobile bind -target=android/arm64 -o streamvault.aar streamvault/core/mobile
package mobile

import (
	"encoding/json"
	"errors"

	"streamvault/core/app"
	"streamvault/core/btnative"
	"streamvault/core/config"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/organizer"
	"streamvault/core/version"
	"streamvault/core/view"
)

// Listener recebe eventos em tempo real (JSON de view.Event). É chamado numa
// goroutine do núcleo: a implementação Kotlin deve mudar de thread para a UI.
type Listener interface {
	OnEvent(eventJSON string)
}

// Core é o núcleo ligado a uma app Android.
type Core struct {
	c      *app.Core
	cancel func()
}

// Version devolve a versão do núcleo.
func Version() string { return version.Version }

// Open abre o núcleo. dbPath e downloadDir devem estar em pastas da app
// (filesDir/getExternalFilesDir): o acesso a pastas escolhidas pelo utilizador
// (SAF) é da camada Kotlin, que copia/move o resultado final.
func Open(dbPath, downloadDir string, l Listener) (*Core, error) {
	if dbPath == "" || downloadDir == "" {
		return nil, errors.New("mobile: dbPath e downloadDir obrigatórios")
	}
	cfg := config.Default()
	cfg.Storage.DefaultDir = downloadDir
	// BitTorrent ativo por defeito (DHT incluída); se a porta de peers falhar
	// o núcleo arranca na mesma, só sem magnet/.torrent.
	var opts []app.Option
	if be, err := btnative.New(btnative.Options{ListenAddr: ":0", DHT: true}); err == nil {
		opts = append(opts, app.WithTorrentBackend(be))
	}
	c, err := app.Open(cfg, dbPath, opts...)
	if err != nil {
		return nil, err
	}
	return wrap(c, l), nil
}

func wrap(c *app.Core, l Listener) *Core {
	m := &Core{c: c, cancel: func() {}}
	if l != nil {
		m.cancel = c.Events.Subscribe(func(e engine.Event) {
			b, _ := json.Marshal(view.FromEvent(e))
			l.OnEvent(string(b))
		})
	}
	return m
}

func asJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// Add reconhece, valida e enfileira um link. Devolve a tarefa em JSON.
func (m *Core) Add(link string) (string, error) {
	t, err := m.c.Add(link, domain.DownloadPolicy{})
	if err != nil {
		return "", err
	}
	if cur, gerr := m.c.Store.Get(t.ID); gerr == nil {
		t = cur
	}
	return asJSON(view.FromTask(t))
}

// Tasks devolve a lista de tarefas em JSON.
func (m *Core) Tasks() (string, error) {
	list, err := m.c.Store.List()
	if err != nil {
		return "", err
	}
	out := make([]view.Task, 0, len(list))
	for _, t := range list {
		out = append(out, view.FromTask(t))
	}
	return asJSON(out)
}

func (m *Core) Pause(id string) error  { return m.c.Orch.Pause(id) }
func (m *Core) Resume(id string) error { return m.c.Orch.Resume(id) }
func (m *Core) Cancel(id string) error { return m.c.Orch.Cancel(id) }
func (m *Core) Retry(id string) error  { return m.c.Retry(id) }

// Remove apaga da lista uma tarefa terminada.
func (m *Core) Remove(id string) error {
	t, err := m.c.Store.Get(id)
	if err != nil {
		return err
	}
	switch t.State {
	case domain.StateCompleted, domain.StateFailed, domain.StateCancelled:
		return m.c.Store.Delete(id)
	}
	return errors.New("mobile: só tarefas terminadas podem ser removidas")
}

// Organize move o resultado para a biblioteca por categorias; devolve JSON
// {action, category, new_path, duplicate_of[]}.
func (m *Core) Organize(id string, keepDuplicates bool) (string, error) {
	res, err := m.c.Organize(id, organizer.Options{BaseDir: m.c.LibraryDir(), MoveDuplicates: keepDuplicates})
	if err != nil {
		return "", err
	}
	dups := make([]string, 0, len(res.DuplicateOf))
	for _, d := range res.DuplicateOf {
		dups = append(dups, d.Path)
	}
	return asJSON(map[string]any{"action": string(res.Action), "category": res.Category,
		"new_path": res.NewPath, "duplicate_of": dups})
}

// Close faz shutdown ordenado (chamar em onDestroy do serviço).
func (m *Core) Close() error {
	m.cancel()
	return m.c.Close()
}
