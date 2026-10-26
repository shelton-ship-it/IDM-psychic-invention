// Package app compõe o núcleo: persistência + motores + orquestrador +
// reconhecimento/segurança de links. É a única "cola" que as interfaces
// (Windows/Android/CLI) precisam de usar — nenhuma lógica de downloads vive
// nas UIs.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"streamvault/core/capture"
	"streamvault/core/config"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/hlsengine"
	"streamvault/core/httpengine"
	"streamvault/core/linkrec"
	"streamvault/core/orchestrator"
	"streamvault/core/organizer"
	"streamvault/core/plugin"
	"streamvault/core/scheduler"
	"streamvault/core/security"
	"streamvault/core/storage"
	"streamvault/core/torrentengine"
)

// Persistence é o que o núcleo precisa do armazenamento. *storage.Store
// implementa-a; testes usam uma versão em memória.
type Persistence interface {
	orchestrator.Store
	SaveResume(rs storage.ResumeState) error
	LoadResume(taskID string) (*storage.ResumeState, error)
	UpsertHash(e storage.HashEntry) error
	FindByHash(sha256hex string) ([]storage.HashEntry, error)
}

// Core é o núcleo montado.
type Core struct {
	Cfg       config.Config
	Store     Persistence
	Orch      *orchestrator.Orchestrator
	Events    *orchestrator.Fanout // UI subscreve aqui
	HTTP      *httpengine.Engine
	HLS       *hlsengine.Engine     // streams HLS (.m3u8) sem cifra, em demanda (ADR-009)
	Scheduler *scheduler.Scheduler  // nil até StartScheduler
	BT        *torrentengine.Engine // nil até existir um Backend (WithTorrentBackend)
	Links     *linkrec.Registry     // adaptadores do núcleo (hls/dash/http/magnet/torrent)
	Plugins   *plugin.Host          // adaptadores compilados, só ativos após aprovação
	LAN       *LANService           // nil até StartLAN

	pumpMu  sync.Mutex
	closed  bool
	secrets *secretStore // credenciais (cookies) só em memória (ADR-010)

	entMu sync.Mutex
	ent   domain.Entitlements
}

// Option personaliza a montagem do núcleo.
type Option func(*settings)

type settings struct {
	approvals plugin.Approvals
	backend   torrentengine.Backend
}

// WithApprovals define onde ficam as aprovações de adaptadores (defeito:
// memória — aprovações perdem-se ao fechar; Open usa um ficheiro).
func WithApprovals(a plugin.Approvals) Option { return func(s *settings) { s.approvals = a } }

// WithTorrentBackend liga o transporte BitTorrent (ex.: adaptador anacrolix).
// Sem Backend, magnets/.torrent falham com erro tipado "unsupported_source".
func WithTorrentBackend(b torrentengine.Backend) Option { return func(s *settings) { s.backend = b } }

// Open abre a base SQLite em dbPath e monta o núcleo. As aprovações de
// adaptadores ficam em <dbPath>.plugins.json.
func Open(cfg config.Config, dbPath string, extra ...Option) (*Core, error) {
	ap, err := plugin.OpenFileApprovals(dbPath + ".plugins.json")
	if err != nil {
		return nil, err
	}
	st, err := storage.Open(storage.Options{Path: dbPath})
	if err != nil {
		return nil, err
	}
	c, err := New(cfg, st, append([]Option{WithApprovals(ap)}, extra...)...)
	if err != nil {
		st.Close()
		return nil, err
	}
	return c, nil
}

// New monta o núcleo sobre uma persistência já aberta.
func New(cfg config.Config, st Persistence, opts ...Option) (*Core, error) {
	var set settings
	for _, o := range opts {
		o(&set)
	}
	if set.approvals == nil {
		set.approvals = plugin.NewMemApprovals()
	}
	if st == nil {
		return nil, errors.New("app: persistência obrigatória")
	}
	c := &Core{Cfg: cfg, Store: st, Events: orchestrator.NewFanout(256), secrets: newSecretStore()}

	c.HTTP = httpengine.New(httpengine.Options{
		Connections: cfg.Network.ConnectionsPerDownload,
		ResumeProvider: func(id string) (*httpengine.ResumeState, bool) {
			rs, err := st.LoadResume(id)
			if err != nil {
				return nil, false // sem estado fiável => recomeçar limpo
			}
			return fromStorageResume(rs)
		},
		ResumePersist: func(rs httpengine.ResumeState) {
			_ = st.SaveResume(toStorageResume(rs))
		},
	})

	c.HLS = hlsengine.New(hlsengine.Options{Connections: cfg.Network.ConnectionsPerDownload})

	router := orchestrator.NewEngineRouter()
	router.Register(domain.SourceHTTP, cookieEngine{Engine: c.HTTP, secrets: c.secrets})
	router.Register(domain.SourceHLS, cookieEngine{Engine: c.HLS, secrets: c.secrets})
	router.Register(domain.SourceDASH, cookieEngine{Engine: c.HLS, secrets: c.secrets}) // o mesmo motor serve DASH (ADR-011)
	if set.backend != nil {
		bt, err := torrentengine.New(torrentengine.Options{Backend: set.backend,
			AllowPrivateNetworks: cfg.Security.AllowPrivateNetworks})
		if err != nil {
			c.HTTP.Close()
			c.HLS.Close()
			c.Events.Close()
			return nil, err
		}
		c.BT = bt
		router.Register(domain.SourceMagnet, bt)
		router.Register(domain.SourceTorrent, bt)
	}

	// Ordem importa: .torrent, magnet, .m3u8 (HLS) e .mpd (DASH) antes do HTTP genérico.
	c.Links = linkrec.NewRegistry()
	c.Links.Register(linkrec.NewTorrentFileAdapter())
	c.Links.Register(linkrec.NewMagnetAdapter())
	c.Links.Register(linkrec.NewHLSAdapter())
	c.Links.Register(linkrec.NewDASHAdapter())
	c.Links.Register(linkrec.NewHTTPAdapter())
	c.Plugins = plugin.NewHost(c.Links, set.approvals)

	orch, err := orchestrator.New(orchestrator.Options{
		Store: st, Router: router, Sink: c.Events, MaxConcur: cfg.Queue.MaxConcurrentTasks,
	})
	if err != nil {
		c.HTTP.Close()
		c.HLS.Close()
		c.Events.Close()
		return nil, err
	}
	c.Orch = orch
	c.Events.Subscribe(c.onEngineEvent)

	if _, err := orch.Recover(); err != nil {
		c.Close()
		return nil, fmt.Errorf("app: recuperação: %w", err)
	}
	return c, nil
}

// onEngineEvent liga motores → orquestrador → índice de hashes → fila.
func (c *Core) onEngineEvent(e engine.Event) {
	if !c.Orch.ObserveEngine(e) {
		return
	}
	if e.Kind == engine.EventCompleted {
		c.indexCompleted(e)
		c.dropTaskSecret(e.TaskID) // concluída: as credenciais deixam de ser precisas
	}
	if c.Cfg.Queue.AutoStart {
		c.Pump()
	}
}

// dropTaskSecret apaga as credenciais em memória de uma tarefa concluída.
func (c *Core) dropTaskSecret(taskID string) {
	if t, err := c.Store.Get(taskID); err == nil {
		if ref := t.Source.Metadata["cookie_ref"]; ref != "" {
			c.secrets.drop(ref)
		}
	}
}

// indexCompleted regista no índice de hashes (duplicados exatos, F9) APENAS
// ficheiros de tarefas realmente concluídas+verificadas.
func (c *Core) indexCompleted(e engine.Event) {
	t, err := c.Store.Get(e.TaskID)
	if err != nil || t.State != domain.StateCompleted {
		return
	}
	sum, path := strings.ToLower(e.Payload["sha256"]), e.Payload["path"]
	if sum == "" || path == "" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	// hash_index tem UMA linha por hash: se já há um ficheiro idêntico ainda
	// presente em disco, ele continua a ser o original canónico — substituí-lo
	// apagaria a prova de que o novo ficheiro é um duplicado (F9).
	if known, err := c.Store.FindByHash(sum); err == nil {
		for _, k := range known {
			if k.Path != path {
				if _, serr := os.Stat(k.Path); serr == nil {
					return
				}
			}
		}
	}
	now := time.Now().UTC()
	_ = c.Store.UpsertHash(storage.HashEntry{SHA256Hex: sum, Path: path,
		SizeBytes: st.Size(), ModTime: st.ModTime().UTC(), VerifiedAt: now, SourceTask: t.ID})
}

// Add reconhece, valida (segurança) e enfileira um link. Nada é descarregado
// de origens bloqueadas pela política; a falha é explicada, não silenciada.
func (c *Core) Add(raw string, policy domain.DownloadPolicy) (*domain.DownloadTask, error) {
	return c.AddWith(raw, policy, Hints{})
}

// Hints são pistas do navegador de onde o utilizador iniciou o download.
type Hints struct {
	Referer   string // página onde o link estava
	UserAgent string // User-Agent do navegador
	// Cookie é o cabeçalho Cookie da sessão do utilizador no site (downloads atrás
	// de login). Fica SÓ em memória, é enviado apenas ao host exato do URL e nunca
	// sai pela API nem pelos logs (ADR-010). Vazio = sem credenciais.
	Cookie string
}

// AddWith é Add com pistas do navegador (Referer/User-Agent), guardadas na origem
// da tarefa (nunca expostas na API nem nos logs).
func (c *Core) AddWith(raw string, policy domain.DownloadPolicy, h Hints) (*domain.DownloadTask, error) {
	src, err := c.Plugins.Recognize(raw)
	if err != nil {
		return nil, err
	}
	webSource := isWebKind(src.Kind)
	if webSource && (h.Referer != "" || h.UserAgent != "") {
		md := map[string]string{}
		for k, v := range src.Metadata {
			md[k] = v
		}
		if h.Referer != "" {
			md["referer"] = h.Referer
		}
		if h.UserAgent != "" {
			md["user_agent"] = h.UserAgent
		}
		src.Metadata = md
	}
	if webSource {
		if err := c.checkWebURL(src.URL); err != nil {
			return nil, err
		}
	}
	cookieRef := ""
	if h.Cookie != "" {
		if err := checkCookieHint(webSource, h.Cookie); err != nil {
			return nil, err
		}
		pu, perr := url.Parse(src.URL)
		if perr != nil || pu.Host == "" {
			return nil, domain.NewError(domain.ErrInvalidInput, perr, "URL inválido para associar credenciais.", "Verifique o link.", domain.CertaintyObserved, false)
		}
		cookieRef = c.secrets.put(h.Cookie)
		md := map[string]string{}
		for k, v := range src.Metadata {
			md[k] = v
		}
		md["cookie_ref"], md["cookie_host"] = cookieRef, pu.Host // só o token e o host são persistidos
		src.Metadata = md
	}
	if policy.DestinationDir == "" {
		policy.DestinationDir = c.Cfg.Storage.DefaultDir
	}
	if policy.SpeedLimitBps == 0 {
		policy.SpeedLimitBps = c.Cfg.Bandwidth.PerTaskLimitBps
	}
	policy.AllowResume = true
	// Torrents: verificação por peças é a prova de integridade por defeito.
	if (src.Kind == domain.SourceMagnet || src.Kind == domain.SourceTorrent) &&
		policy.RequireIntegrity == domain.IntegrityNone {
		policy.RequireIntegrity = domain.IntegrityTorrentPieces
	}
	t, err := c.Orch.Submit(src, policy)
	if err != nil {
		if cookieRef != "" {
			c.secrets.drop(cookieRef)
		}
		return nil, err
	}
	if c.Cfg.Queue.AutoStart {
		c.Pump()
	}
	return t, nil
}

// isWebKind: origens que são um URL http(s) (segurança, redirecionamentos e cookies).
func isWebKind(k domain.SourceKind) bool {
	return k == domain.SourceHTTP || k == domain.SourceHLS || k == domain.SourceDASH
}

// checkWebURL aplica a política de segurança a um URL http(s)/HLS.
func (c *Core) checkWebURL(raw string) error {
	opts := security.DefaultOptions()
	opts.AllowPrivateNetworks = c.Cfg.Security.AllowPrivateNetworks
	for _, f := range security.CheckURL(raw, opts) {
		if f.Severity == "block" {
			return domain.NewError(domain.ErrPolicyViolation, nil, f.Message,
				"Use uma origem pública ou confirme explicitamente redes privadas nas definições.",
				domain.CertaintyObserved, false)
		}
	}
	return nil
}

// checkCookieHint valida um cookie recebido (só web, formato seguro).
func checkCookieHint(webSource bool, cookie string) error {
	if !webSource {
		return domain.NewError(domain.ErrInvalidInput, nil, "Cookies só se aplicam a ligações http(s) e HLS.",
			"Remova o cookie ou use um link de página/ficheiro.", domain.CertaintyObserved, false)
	}
	if !validCookie(cookie) {
		return domain.NewError(domain.ErrInvalidInput, nil, "O cookie é inválido (vazio, demasiado longo ou com caracteres de controlo).",
			"Reenvie o cabeçalho Cookie tal como o navegador o mostra.", domain.CertaintyObserved, false)
	}
	return nil
}

// Validate diz se AddWith aceitaria este link, sem o enfileirar nem descarregar.
func (c *Core) Validate(raw string, h Hints) error {
	src, err := c.Plugins.Recognize(raw)
	if err != nil {
		return err
	}
	web := isWebKind(src.Kind)
	if web {
		if err := c.checkWebURL(src.URL); err != nil {
			return err
		}
	}
	if h.Cookie != "" {
		return checkCookieHint(web, h.Cookie)
	}
	return nil
}

// Pump inicia tarefas pendentes até esgotar a capacidade.
func (c *Core) Pump() {
	c.pumpMu.Lock()
	defer c.pumpMu.Unlock()
	if c.closed {
		return
	}
	for i := 0; i < 1000; i++ {
		t, _ := c.Orch.StartNext()
		if t == nil {
			return
		}
	}
}

// Close faz shutdown ordenado.
func (c *Core) Close() error {
	c.pumpMu.Lock()
	if c.closed {
		c.pumpMu.Unlock()
		return nil
	}
	c.closed = true
	c.pumpMu.Unlock()
	if c.Scheduler != nil {
		c.Scheduler.Close() // primeiro: nada novo arranca durante o shutdown
	}
	c.Orch.Shutdown(context.Background())
	c.secrets.wipe()
	c.HTTP.Close()
	c.HLS.Close()
	if c.LAN != nil {
		c.LAN.Close()
	}
	if c.BT != nil {
		c.BT.Close()
	}
	c.Events.Close()
	if cl, ok := c.Store.(interface{ Close() error }); ok {
		return cl.Close()
	}
	return nil
}

// Organize classifica e move o ficheiro de uma tarefa CONCLUÍDA para a pasta da
// sua categoria. Se existir cópia idêntica (índice de hashes), devolve
// ActionDuplicate sem mover nem apagar: a decisão é do utilizador (passar
// MoveDuplicates=true em o para manter ambas).
func (c *Core) Organize(taskID string, o organizer.Options) (organizer.Result, error) {
	t, err := c.Store.Get(taskID)
	if err != nil {
		return organizer.Result{}, err
	}
	if t.State != domain.StateCompleted {
		return organizer.Result{}, fmt.Errorf("app: só tarefas concluídas são organizadas (%s está %s)", taskID, t.State)
	}
	var fm *domain.FileMetadata
	for i := range t.Files {
		if t.Files[i].Index == 0 && t.Files[i].Path != "" {
			fm = &t.Files[i]
		}
	}
	if fm == nil {
		return organizer.Result{}, errors.New("app: tarefa sem ficheiro final registado")
	}
	dir := t.Policy.DestinationDir
	if dir == "" {
		dir = "."
	}
	oldPath := filepath.Join(dir, fm.Path)
	if o.Lookup == nil {
		o.Lookup = func(sha string) ([]organizer.Known, error) {
			entries, err := c.Store.FindByHash(sha)
			if err != nil {
				return nil, err
			}
			out := make([]organizer.Known, 0, len(entries))
			for _, e := range entries {
				out = append(out, organizer.Known{Path: e.Path, SizeBytes: e.SizeBytes})
			}
			return out, nil
		}
	}
	res, err := organizer.Organize(oldPath, "", fm.SHA256Hex, o)
	if err != nil || res.Action != organizer.ActionMoved || res.NewPath == oldPath {
		return res, err
	}
	// atualizar tarefa e índice para o novo local
	t.Policy.DestinationDir = filepath.Dir(res.NewPath)
	fm.Path = filepath.Base(res.NewPath)
	if err := c.Store.Save(t); err != nil {
		return res, err
	}
	if sum := strings.ToLower(fm.SHA256Hex); sum != "" {
		if known, ferr := c.Store.FindByHash(sum); ferr == nil {
			for _, k := range known {
				if k.Path == oldPath {
					k.Path = res.NewPath
					_ = c.Store.UpsertHash(k)
				}
			}
		}
	}
	return res, nil
}

// LibraryDir é a raiz da organização por categorias (config ou <default>/Biblioteca).
func (c *Core) LibraryDir() string {
	if c.Cfg.Storage.LibraryDir != "" {
		return c.Cfg.Storage.LibraryDir
	}
	base := c.Cfg.Storage.DefaultDir
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "Biblioteca")
}

// Retry reenvia uma tarefa Failed para a fila (transição válida Failed→Pending).
func (c *Core) Retry(id string) error {
	t, err := c.Store.Get(id)
	if err != nil {
		return err
	}
	if err := t.Transition(domain.StatePending); err != nil {
		return err
	}
	t.Error = nil
	if err := c.Store.Save(t); err != nil {
		return err
	}
	c.Pump()
	return nil
}

// Capture procura mídia descarregável numa página (como o IDM). Só devolve
// URLs que a própria página expõe; HLS (.m3u8) é suportado, DASH (.mpd) vem marcado como não suportado.
func (c *Core) Capture(ctx context.Context, pageURL string, h Hints) (*capture.Result, error) {
	return capture.Find(ctx, pageURL, capture.Options{
		AllowPrivateNetworks: c.Cfg.Security.AllowPrivateNetworks, UserAgent: h.UserAgent, Referer: h.Referer,
		Cookie: h.Cookie})
}

// CaptureLinks lista as ligações para ficheiros descarregáveis de uma página
// (e, se pedido, das páginas seguintes por rel="next", no mesmo host): o
// "grabber" do IDM. Mesma política de segurança e de credenciais que Capture.
func (c *Core) CaptureLinks(ctx context.Context, pageURL string, h Hints, lo capture.LinkOptions) (*capture.LinksResult, error) {
	lo.Options = capture.Options{AllowPrivateNetworks: c.Cfg.Security.AllowPrivateNetworks,
		UserAgent: h.UserAgent, Referer: h.Referer, Cookie: h.Cookie}
	return capture.FindLinks(ctx, pageURL, lo)
}
