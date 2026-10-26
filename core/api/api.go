// Package api expõe o núcleo por HTTP/JSON local (Fase 6, ADR-003). A UI
// Windows (e qualquer outra) é um cliente: nenhuma regra de downloads vive nela.
//
// Segurança (o servidor só aceita pedidos legítimos do próprio utilizador):
//   - escuta apenas em loopback (ValidateListenAddr);
//   - token Bearer obrigatório em /v1/* (comparação em tempo constante);
//   - o cabeçalho Host tem de ser loopback (defesa contra DNS rebinding);
//   - se houver Origin, tem de ser a própria origem (defesa contra CSRF);
//   - corpos limitados e JSON estrito; a URL completa nunca é devolvida.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"streamvault/core/app"
	"streamvault/core/capture"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/organizer"
	"streamvault/core/scheduler"
	"streamvault/core/version"
	"streamvault/core/view"
)

//go:embed ui/index.html
var uiFS embed.FS

const maxBody = 1 << 20

// ValidateListenAddr recusa qualquer endereço que não seja loopback.
func ValidateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("api: endereço inválido %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("api: só é permitido escutar em loopback (recebido %q)", host)
	}
	return nil
}

// Server é o handler HTTP.
type Server struct {
	core  *app.Core
	token string
	mux   *http.ServeMux
}

// New cria o handler. token deve ter entropia suficiente (>= 16 bytes).
func New(core *app.Core, token string) (*Server, error) {
	if len(token) < 16 {
		return nil, errors.New("api: token demasiado curto (mínimo 16 caracteres)")
	}
	s := &Server{core: core, token: token, mux: http.NewServeMux()}
	s.mux.HandleFunc("/v1/", s.v1)
	s.mux.HandleFunc("/", s.ui)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r.Host) {
		http.Error(w, "host não permitido", http.StatusForbidden)
		return
	}
	o := r.Header.Get("Origin")
	ext := extensionOrigin(o)
	// Origin de páginas web só é aceite se for a própria origem (anti-CSRF).
	// Origens de extensões de navegador são aceites: uma página web não consegue
	// forjá-las, e o token continua obrigatório em /v1/*.
	if o != "" && o != "http://"+r.Host && !ext {
		http.Error(w, "origem não permitida", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if ext {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions { // pré-voo: sem dados, sem token
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// extensionOrigin reconhece chrome-extension://<id>, moz-extension://<id> e
// safari-web-extension://<id> (formato estrito: só o esquema e o identificador).
func extensionOrigin(o string) bool {
	for _, p := range []string{"chrome-extension://", "moz-extension://", "safari-web-extension://"} {
		if strings.HasPrefix(o, p) {
			id := o[len(p):]
			return id != "" && len(id) <= 64 && !strings.ContainsAny(id, "/?# \t\r\n:@")
		}
	}
	return false
}

func loopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, _ := uiFS.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	w.Write(data)
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(p):]), []byte(s.token)) == 1
}

// ---- roteamento ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	if de, ok := err.(*domain.DownloadError); ok {
		writeJSON(w, code, map[string]any{"error": view.FromError(de)})
		return
	}
	writeJSON(w, code, map[string]any{"error": view.Err{Class: "request", Message: err.Error()}})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("corpo inválido: %w", err)
	}
	return nil
}

func (s *Server) v1(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeErr(w, http.StatusUnauthorized, errors.New("token em falta ou inválido"))
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "health" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"ok": true, "version": version.Version})
	case len(parts) == 1 && parts[0] == "config" && r.Method == http.MethodGet:
		c := s.core.Cfg.Commercial
		writeJSON(w, 200, map[string]any{"product_id": c.ProductID, "edition": c.Edition, "price_brl_cents": c.PriceBRLCents})
	case len(parts) == 1 && parts[0] == "tasks":
		s.tasks(w, r)
	case len(parts) == 2 && parts[0] == "tasks" && r.Method == http.MethodDelete:
		s.deleteTask(w, parts[1])
	case len(parts) == 3 && parts[0] == "tasks" && r.Method == http.MethodPost:
		s.taskAction(w, r, parts[1], parts[2])
	case len(parts) == 1 && parts[0] == "capture" && r.Method == http.MethodPost:
		s.capture(w, r)
	case len(parts) == 1 && parts[0] == "schedule":
		s.schedule(w, r)
	case len(parts) == 2 && parts[0] == "schedule" && r.Method == http.MethodDelete:
		s.unschedule(w, parts[1])
	case len(parts) == 2 && parts[0] == "capture" && parts[1] == "links" && r.Method == http.MethodPost:
		s.captureLinks(w, r)
	case len(parts) == 2 && parts[0] == "tasks" && parts[1] == "batch" && r.Method == http.MethodPost:
		s.addBatch(w, r)
	case len(parts) == 1 && parts[0] == "plugins" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.core.Plugins.List())
	case len(parts) == 3 && parts[0] == "plugins" && r.Method == http.MethodPost:
		s.pluginAction(w, parts[1], parts[2])
	case len(parts) == 2 && parts[0] == "lan" && parts[1] == "pairing" && r.Method == http.MethodPost:
		pin, exp, err := s.core.LANPairing()
		if err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"pin": pin, "expires": exp.UTC().Format(time.RFC3339), "addr": s.core.LAN.Addr})
	case len(parts) == 1 && parts[0] == "events" && r.Method == http.MethodGet:
		s.events(w, r)
	default:
		writeErr(w, http.StatusNotFound, errors.New("rota inexistente"))
	}
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.core.Store.List()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out := make([]view.Task, 0, len(list))
		for _, t := range list {
			out = append(out, view.FromTask(t))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in struct {
			URL       string `json:"url"`
			Referer   string `json:"referer"`
			UserAgent string `json:"user_agent"`
			Cookie    string `json:"cookie"` // credenciais da sessão: só em memória, nunca devolvidas (ADR-010)
		}
		if err := decode(w, r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if strings.TrimSpace(in.URL) == "" {
			writeErr(w, 400, errors.New("url obrigatória"))
			return
		}
		t, err := s.core.AddWith(in.URL, domain.DownloadPolicy{}, app.Hints{Referer: in.Referer, UserAgent: in.UserAgent, Cookie: in.Cookie})
		if err != nil {
			code := 422
			if de, ok := err.(*domain.DownloadError); ok && de.Class == domain.ErrPolicyViolation {
				code = 403
			}
			writeErr(w, code, err)
			return
		}
		cur, gerr := s.core.Store.Get(t.ID)
		if gerr != nil {
			cur = t
		}
		writeJSON(w, 201, view.FromTask(cur))
	default:
		writeErr(w, 405, errors.New("método não permitido"))
	}
}

func (s *Server) deleteTask(w http.ResponseWriter, id string) {
	t, err := s.core.Store.Get(id)
	if err != nil {
		writeErr(w, 404, errors.New("tarefa inexistente"))
		return
	}
	switch t.State {
	case domain.StateCompleted, domain.StateFailed, domain.StateCancelled:
	default:
		writeErr(w, 409, fmt.Errorf("só tarefas terminadas podem ser removidas (%s está %s)", id, t.State))
		return
	}
	if err := s.core.Store.Delete(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) taskAction(w http.ResponseWriter, r *http.Request, id, action string) {
	var err error
	switch action {
	case "pause":
		err = s.core.Orch.Pause(id)
	case "resume":
		err = s.core.Orch.Resume(id)
	case "cancel":
		err = s.core.Orch.Cancel(id)
	case "retry":
		err = s.core.Retry(id)
	case "organize":
		var in struct {
			KeepDuplicates bool `json:"keep_duplicates"`
		}
		if derr := decode(w, r, &in); derr != nil {
			writeErr(w, 400, derr)
			return
		}
		res, oerr := s.core.Organize(id, organizer.Options{BaseDir: s.core.LibraryDir(), MoveDuplicates: in.KeepDuplicates})
		if oerr != nil {
			writeErr(w, 409, oerr)
			return
		}
		dups := make([]string, 0, len(res.DuplicateOf))
		for _, d := range res.DuplicateOf {
			dups = append(dups, d.Path)
		}
		writeJSON(w, 200, map[string]any{"action": string(res.Action), "category": res.Category,
			"new_path": res.NewPath, "duplicate_of": dups})
		return
	default:
		writeErr(w, 404, errors.New("ação inexistente"))
		return
	}
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	t, gerr := s.core.Store.Get(id)
	if gerr != nil {
		writeErr(w, 404, gerr)
		return
	}
	writeJSON(w, 200, view.FromTask(t))
}

func (s *Server) pluginAction(w http.ResponseWriter, name, action string) {
	var err error
	switch action {
	case "approve":
		err = s.core.Plugins.Approve(name)
	case "revoke":
		err = s.core.Plugins.Revoke(name)
	default:
		writeErr(w, 404, errors.New("ação inexistente"))
		return
	}
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	writeJSON(w, 200, s.core.Plugins.List())
}

// events envia eventos em Server-Sent Events. Cada cliente tem a sua
// subscrição, cancelada ao desligar; um cliente lento perde eventos antigos
// (buffer pequeno) em vez de bloquear o núcleo.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming não suportado"))
		return
	}
	ch := make(chan engine.Event, 64)
	var once sync.Once
	cancel := s.core.Events.Subscribe(func(e engine.Event) {
		select {
		case ch <- e:
		default: // cliente lento: descarta
		}
	})
	defer once.Do(cancel)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	fmt.Fprint(w, ": ligado\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(view.FromEvent(e))
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// capture procura mídia numa página: {"url":"…","referer":"…","user_agent":"…"}.
func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL       string `json:"url"`
		Referer   string `json:"referer"`
		UserAgent string `json:"user_agent"`
		Cookie    string `json:"cookie"` // página atrás de login; nunca devolvido nem registado
	}
	if err := decode(w, r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(in.URL) == "" || len(in.URL) > 4096 {
		writeErr(w, 400, errors.New("url inválida"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res, err := s.core.Capture(ctx, in.URL, app.Hints{Referer: in.Referer, UserAgent: in.UserAgent, Cookie: in.Cookie})
	if err != nil {
		writeErr(w, 422, err)
		return
	}
	writeJSON(w, 200, res)
}

// captureLinks lista os ficheiros descarregáveis de uma página (grabber):
// {"url","referer","user_agent","cookie","categories":[…],"extensions":[…],
// "contains":"…","max_pages":N,"max_links":N}.
func (s *Server) captureLinks(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL        string   `json:"url"`
		Referer    string   `json:"referer"`
		UserAgent  string   `json:"user_agent"`
		Cookie     string   `json:"cookie"`
		Categories []string `json:"categories"`
		Extensions []string `json:"extensions"`
		Contains   string   `json:"contains"`
		MaxPages   int      `json:"max_pages"`
		MaxLinks   int      `json:"max_links"`
	}
	if err := decode(w, r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(in.URL) == "" || len(in.URL) > 4096 {
		writeErr(w, 400, errors.New("url inválida"))
		return
	}
	// cada página pode demorar até ao tempo limite do cliente HTTP (20 s)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.core.CaptureLinks(ctx, in.URL,
		app.Hints{Referer: in.Referer, UserAgent: in.UserAgent, Cookie: in.Cookie},
		capture.LinkOptions{Categories: in.Categories, Extensions: in.Extensions, Contains: in.Contains,
			MaxPages: in.MaxPages, MaxLinks: in.MaxLinks})
	if err != nil {
		writeErr(w, 422, err)
		return
	}
	if res.Links == nil {
		res.Links = []capture.Link{}
	}
	writeJSON(w, 200, res)
}

// MaxBatch limita as ligações de um pedido em lote.
const MaxBatch = 200

// addBatch junta várias ligações de uma vez (típico: as escolhidas no grabber):
// {"urls":[…],"referer","user_agent","cookie","cookie_url"}. O cookie só é
// enviado às ligações cujo host é IGUAL ao de cookie_url (o site onde a sessão
// foi iniciada); as restantes entram sem credenciais.
func (s *Server) addBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URLs      []string `json:"urls"`
		Referer   string   `json:"referer"`
		UserAgent string   `json:"user_agent"`
		Cookie    string   `json:"cookie"`
		CookieURL string   `json:"cookie_url"`
	}
	if err := decode(w, r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(in.URLs) == 0 || len(in.URLs) > MaxBatch {
		writeErr(w, 400, fmt.Errorf("envie entre 1 e %d ligações", MaxBatch))
		return
	}
	cookieHost := ""
	if in.Cookie != "" {
		cu, err := url.Parse(strings.TrimSpace(in.CookieURL))
		if err != nil || cu.Host == "" || (cu.Scheme != "http" && cu.Scheme != "https") {
			writeErr(w, 400, errors.New("com cookie, indique cookie_url: o endereço do site onde a sessão foi iniciada"))
			return
		}
		cookieHost = cu.Host
	}
	type item struct {
		URL           string `json:"url"`
		OK            bool   `json:"ok"`
		ID            string `json:"id,omitempty"`
		Error         string `json:"error,omitempty"`
		CookieApplied bool   `json:"cookie_applied,omitempty"`
	}
	out := make([]item, 0, len(in.URLs))
	for _, raw := range in.URLs {
		it := item{URL: domain.DownloadSource{URL: raw}.Redacted()}
		if strings.TrimSpace(raw) == "" || len(raw) > 4096 {
			it.Error = "url inválida"
			out = append(out, it)
			continue
		}
		h := app.Hints{Referer: in.Referer, UserAgent: in.UserAgent}
		if cookieHost != "" {
			if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && strings.EqualFold(u.Host, cookieHost) {
				h.Cookie, it.CookieApplied = in.Cookie, true
			}
		}
		t, err := s.core.AddWith(raw, domain.DownloadPolicy{}, h)
		if err != nil {
			it.Error = err.Error()
		} else {
			it.OK, it.ID = true, t.ID
		}
		out = append(out, it)
	}
	writeJSON(w, 200, map[string]any{"results": out})
}

// scheduleView é o que a API mostra de uma entrada agendada: o URL vai sem query
// (pode ter tokens) e nunca há credenciais.
type scheduleView struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	At          string `json:"at"`
	CreatedAt   string `json:"created_at"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	NeedsCookie bool   `json:"needs_cookie,omitempty"`
}

func toScheduleView(e scheduler.Entry) scheduleView {
	return scheduleView{ID: e.ID, URL: domain.DownloadSource{URL: e.URL}.Redacted(),
		At: e.At.UTC().Format(time.RFC3339), CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		Status: string(e.Status), Error: e.Error, NeedsCookie: e.NeedsCookie}
}

// schedule: GET lista; POST {"url","at":"RFC 3339","referer","user_agent","cookie"} agenda.
func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	if s.core.Scheduler == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("o agendador não está ativo"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := s.core.Scheduler.List()
		out := make([]scheduleView, 0, len(list))
		for _, e := range list {
			out = append(out, toScheduleView(e))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in struct {
			URL       string `json:"url"`
			At        string `json:"at"`
			Referer   string `json:"referer"`
			UserAgent string `json:"user_agent"`
			Cookie    string `json:"cookie"`
		}
		if err := decode(w, r, &in); err != nil {
			writeErr(w, 400, err)
			return
		}
		if strings.TrimSpace(in.URL) == "" || len(in.URL) > 4096 {
			writeErr(w, 400, errors.New("url inválida"))
			return
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(in.At))
		if err != nil {
			writeErr(w, 400, errors.New("hora inválida: use o formato RFC 3339, por exemplo 2026-10-25T02:00:00Z"))
			return
		}
		e, err := s.core.Schedule(in.URL, at, app.Hints{Referer: in.Referer, UserAgent: in.UserAgent, Cookie: in.Cookie})
		if err != nil {
			code := 422
			if de, ok := err.(*domain.DownloadError); ok {
				switch de.Class {
				case domain.ErrPolicyViolation:
					code = 403
				case domain.ErrInvalidInput:
					code = 400
				}
			}
			writeErr(w, code, err)
			return
		}
		writeJSON(w, 201, toScheduleView(e))
	default:
		writeErr(w, 405, errors.New("método não permitido"))
	}
}

func (s *Server) unschedule(w http.ResponseWriter, id string) {
	if s.core.Scheduler == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("o agendador não está ativo"))
		return
	}
	if err := s.core.Scheduler.Remove(id); err != nil {
		code := 404
		if strings.Contains(err.Error(), "a arrancar") {
			code = 409
		}
		writeErr(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
