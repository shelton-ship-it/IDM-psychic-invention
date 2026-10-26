package hlsengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"streamvault/core/diag"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/httpengine"
	"streamvault/core/mp4merge"
	"streamvault/core/security"
)

// Options do motor HLS.
type Options struct {
	Client         *http.Client
	UserAgent      string
	Connections    int           // segmentos em simultâneo; defeito 6 (máx. 16)
	SegmentRetries int           // defeito 5
	SegmentBackoff time.Duration // defeito 500 ms
	StallTimeout   time.Duration // sem bytes => repetir; defeito 30 s
	ProgressEvery  time.Duration // defeito 500 ms
	// Window: quantos segmentos à frente do ponto de escrita podem estar a ser
	// descarregados (limita o espaço temporário); defeito 4× ligações + 16.
	Window int
}

// Engine implementa engine.Engine para SourceHLS.
type Engine struct {
	opts    Options
	client  *http.Client
	mu      sync.Mutex
	handles map[string]*job
	closed  bool
}

var _ engine.Engine = (*Engine)(nil)

func New(o Options) *Engine {
	if o.Connections <= 0 {
		o.Connections = 6
	}
	if o.Connections > 16 {
		o.Connections = 16
	}
	if o.SegmentRetries <= 0 {
		o.SegmentRetries = 5
	}
	if o.SegmentBackoff <= 0 {
		o.SegmentBackoff = 500 * time.Millisecond
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = 30 * time.Second
	}
	if o.ProgressEvery <= 0 {
		o.ProgressEvery = 500 * time.Millisecond
	}
	if o.UserAgent == "" {
		o.UserAgent = "StreamVaultCore/1 (hls)"
	}
	hc := o.Client
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 16,
			ResponseHeaderTimeout: 30 * time.Second}}
	}
	cl := *hc
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return errors.New("demasiados redirecionamentos")
		}
		return httpengine.CheckRedirect(via, req)
	}
	return &Engine{opts: o, client: &cl, handles: map[string]*job{}}
}

func (e *Engine) Name() string { return "hls" }

func (e *Engine) Capabilities() engine.Capabilities {
	return engine.Capabilities{SupportsPause: true, SupportsResume: true, SupportsSegmentation: true,
		NeedsNetwork: true, EngineVersion: "1"}
}

// CanHandle: HLS (.m3u8) e DASH (.mpd). O mesmo descarregador serve os dois: ambos
// resultam em listas de segmentos por faixa (ADR-011).
func (e *Engine) CanHandle(src domain.DownloadSource) bool {
	return src.Kind == domain.SourceHLS || src.Kind == domain.SourceDASH
}

func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	jobs := make([]*job, 0, len(e.handles))
	for _, j := range e.handles {
		jobs = append(jobs, j)
	}
	e.mu.Unlock()
	for _, j := range jobs {
		j.cancelJob()
	}
	for _, j := range jobs {
		<-j.done
	}
	return nil
}

type job struct {
	id     string
	task   domain.DownloadTask
	sink   engine.EventSink
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error

	mu        sync.Mutex
	paused    bool
	cancelled bool

	bytesDone  int64
	bytesTotal int64
	speedBps   uint64
}

func (j *job) TaskID() string { return j.id }
func (j *job) Pause(context.Context) error {
	j.mu.Lock()
	if j.cancelled {
		j.mu.Unlock()
		return errors.New("hlsengine: tarefa cancelada")
	}
	j.paused = true
	j.mu.Unlock()
	j.cancel()
	return nil
}
func (j *job) Resume(context.Context) error {
	return errors.New("hlsengine: use Orquestrador.Resume (novo Start; o estado fica em disco)")
}
func (j *job) Cancel(context.Context) error { j.cancelJob(); return nil }
func (j *job) cancelJob() {
	j.mu.Lock()
	j.cancelled = true
	j.mu.Unlock()
	j.cancel()
}
func (j *job) isPaused() bool    { j.mu.Lock(); defer j.mu.Unlock(); return j.paused }
func (j *job) isCancelled() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.cancelled }
func (j *job) Snapshot() domain.ProgressSnapshot {
	return domain.ProgressSnapshot{TaskID: j.id, BytesDone: atomic.LoadInt64(&j.bytesDone),
		BytesTotal: atomic.LoadInt64(&j.bytesTotal), SpeedBps: float64(atomic.LoadUint64(&j.speedBps)), UpdatedAt: time.Now().UTC()}
}
func (j *job) emit(kind engine.EventKind, payload map[string]string) {
	if j.sink != nil {
		j.sink.Emit(engine.Event{Kind: kind, TaskID: j.id, Progress: j.Snapshot(),
			Timestamp: time.Now().UTC().UnixMilli(), Payload: payload})
	}
}

// Start arranca a tarefa em goroutine própria.
func (e *Engine) Start(ctx context.Context, req engine.StartRequest) (engine.JobHandle, error) {
	if !e.CanHandle(req.Task.Source) {
		return nil, fmt.Errorf("hlsengine: origem %q não suportada", req.Task.Source.Kind)
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("hlsengine: motor fechado")
	}
	if prev, dup := e.handles[req.Task.ID]; dup {
		if !prev.isPaused() {
			e.mu.Unlock()
			return nil, fmt.Errorf("hlsengine: tarefa %s já ativa", req.Task.ID)
		}
		e.mu.Unlock()
		select {
		case <-prev.done:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("hlsengine: tarefa %s ainda a pausar", req.Task.ID)
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return nil, errors.New("hlsengine: motor fechado")
		}
		if _, dup2 := e.handles[req.Task.ID]; dup2 {
			e.mu.Unlock()
			return nil, fmt.Errorf("hlsengine: tarefa %s já ativa", req.Task.ID)
		}
	}
	jctx, cancel := context.WithCancel(ctx)
	j := &job{id: req.Task.ID, task: req.Task, sink: req.Sink, ctx: jctx, cancel: cancel,
		done: make(chan struct{}), bytesTotal: -1}
	e.handles[j.id] = j
	e.mu.Unlock()
	go func() {
		defer close(j.done)
		defer func() {
			e.mu.Lock()
			delete(e.handles, j.id)
			e.mu.Unlock()
		}()
		j.err = e.run(j)
		if j.err != nil && !j.isPaused() && !j.isCancelled() && j.sink != nil {
			de, ok := j.err.(*domain.DownloadError)
			if !ok {
				de = diag.Explain(j.err, j.task.Source.Redacted())
			}
			j.sink.Emit(engine.Event{Kind: engine.EventError, TaskID: j.id, Progress: j.Snapshot(),
				Error: de, Timestamp: time.Now().UTC().UnixMilli()})
		}
	}()
	return j, nil
}

func unsupported(msg, action string) *domain.DownloadError {
	return domain.NewError(domain.ErrUnsupportedSource, nil, msg, action, domain.CertaintyObserved, false)
}

// ---- rede ----

func (e *Engine) get(ctx context.Context, j *job, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", e.opts.UserAgent)
	httpengine.ApplyBrowserHints(req, j.task.Source.Metadata)
	return e.client.Do(req)
}

func statusError(code int, retryableDefault bool) *domain.DownloadError {
	switch code {
	case 401, 403, 410:
		return domain.NewError(domain.ErrURLExpired, nil,
			fmt.Sprintf("Servidor recusou o acesso (HTTP %d) — o link provavelmente expirou.", code),
			"Obtenha um novo link na origem e reintroduza-o.", domain.CertaintyObserved, false)
	case 404:
		return domain.NewError(domain.ErrHTTPStatus, nil, "Um segmento já não existe no servidor (HTTP 404).",
			"O vídeo pode ter sido removido ou o link expirou; obtenha um novo link.", domain.CertaintyObserved, false)
	}
	return domain.NewError(domain.ErrHTTPStatus, nil, fmt.Sprintf("HTTP %d no pedido à origem.", code),
		"Confirme o link; se persistir, a origem está indisponível.", domain.CertaintyObserved, code >= 500 || retryableDefault)
}

func (e *Engine) fetchText(ctx context.Context, j *job, rawURL string) (string, error) {
	resp, err := e.get(ctx, j, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError(resp.StatusCode, false)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxPlaylistBytes {
		return "", unsupported("A playlist é demasiado grande.", "Este stream não é suportado.")
	}
	return string(b), nil
}

// urlGuard aplica a política anti-SSRF aos URLs que a PLAYLIST aponta: se a
// playlist veio de uma origem pública, ela não pode mandar-nos para a rede interna.
type urlGuard struct {
	allowPrivate bool
	seen         map[string]error
}

func newGuard(playlistURL string) *urlGuard {
	return &urlGuard{allowPrivate: security.IsPrivateOrigin(playlistURL), seen: map[string]error{}}
}

func (g *urlGuard) check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if err, ok := g.seen[u.Host]; ok {
		return err
	}
	opts := security.DefaultOptions()
	opts.AllowPrivateNetworks = g.allowPrivate
	var res error
	for _, f := range security.CheckURL(raw, opts) {
		if f.Severity == "block" {
			res = domain.NewError(domain.ErrPolicyViolation, nil,
				"A playlist aponta para um endereço bloqueado: "+f.Message,
				"Origem recusada por segurança.", domain.CertaintyObserved, false)
			break
		}
	}
	g.seen[u.Host] = res
	return res
}

// loadMedia obtém a media playlist final (resolvendo uma master) e valida-a.
func (e *Engine) loadMedia(j *job, guard *urlGuard) (*Playlist, string, *Rendition, error) {
	cur := j.task.Source.URL
	var audio *Rendition
	for depth := 0; depth < 3; depth++ {
		base, err := url.Parse(cur)
		if err != nil {
			return nil, "", nil, err
		}
		text, err := e.fetchText(j.ctx, j, cur)
		if err != nil {
			return nil, "", nil, err
		}
		p, err := Parse(text, base)
		if err != nil {
			return nil, "", nil, unsupported("Playlist HLS inválida: "+err.Error(), "Confirme o link da playlist.")
		}
		if !p.Master {
			return p, cur, audio, nil
		}
		v, err := p.pickVariant()
		if err != nil {
			return nil, "", nil, unsupported(err.Error(), "Este stream não é suportado.")
		}
		if err := guard.check(v.URL); err != nil {
			return nil, "", nil, err
		}
		audio = p.pickAudio(v)
		if audio != nil {
			if err := guard.check(audio.URL); err != nil {
				return nil, "", nil, err
			}
		}
		cur = v.URL
	}
	return nil, "", nil, unsupported("Playlist HLS com aninhamento excessivo.", "Este stream não é suportado.")
}

// track é uma faixa a descarregar (HLS: variante de vídeo e, se existir, áudio em
// separado; DASH: a representação de vídeo e a de áudio).
type track struct {
	media *Playlist
	url   string // de onde veio (nome do ficheiro e diagnóstico)
}

// loadTracks resolve o link da tarefa nas faixas a descarregar, [vídeo, áudio?].
func (e *Engine) loadTracks(j *job, guard *urlGuard) ([]track, error) {
	if j.task.Source.Kind == domain.SourceDASH {
		base, err := url.Parse(j.task.Source.URL)
		if err != nil {
			return nil, err
		}
		text, err := e.fetchText(j.ctx, j, j.task.Source.URL)
		if err != nil {
			return nil, err
		}
		pls, err := ParseMPD(text, base)
		if err != nil {
			if de, ok := err.(*domain.DownloadError); ok {
				return nil, de
			}
			return nil, unsupported("Manifesto DASH inválido: "+err.Error(), "Confirme o link do manifesto.")
		}
		var out []track
		for _, p := range pls {
			u := j.task.Source.URL
			if len(p.Segments) > 0 {
				u = p.Segments[0].URL
			}
			out = append(out, track{media: p, url: u})
		}
		return out, nil
	}
	media, mediaURL, audio, err := e.loadMedia(j, guard)
	if err != nil {
		return nil, err
	}
	out := []track{{media: media, url: mediaURL}}
	if audio != nil {
		text, err := e.fetchText(j.ctx, j, audio.URL)
		if err != nil {
			return nil, err
		}
		ap, err := Parse(text, mustURL(audio.URL))
		if err != nil || ap.Master {
			return nil, unsupported("A playlist de áudio em separado é inválida.", "Este stream não é suportado.")
		}
		out = append(out, track{media: ap, url: audio.URL})
	}
	return out, nil
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return &url.URL{}
	}
	return u
}

// ---- estado em disco ----

type state struct {
	PlaylistSHA   string `json:"playlist_sha256"`
	N             int    `json:"segments"`
	Name          string `json:"name"`
	Appended      int    `json:"appended"`       // segmentos já anexados e sincronizados
	AppendedBytes int64  `json:"appended_bytes"` // tamanho do ficheiro de saída nesse ponto
	InitDone      bool   `json:"init_done"`
	InitBytes     int64  `json:"init_bytes"`
	Done          bool   `json:"done"` // faixa totalmente descarregada (várias faixas: não repetir)
}

func (s *state) save(dir string) error {
	b, _ := json.Marshal(s)
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

func loadState(dir string) *state {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil
	}
	var s state
	if json.Unmarshal(b, &s) != nil || s.Appended < 0 || s.AppendedBytes < 0 {
		return nil
	}
	return &s
}

func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

func outputExt(p *Playlist) string {
	if p.Ext != "" {
		return p.Ext
	}
	if p.MapURL != "" {
		return ".mp4" // fMP4: init + fragmentos concatenados
	}
	all := func(suffix ...string) bool {
		for _, s := range p.Segments {
			u, _ := url.Parse(s.URL)
			ok := false
			for _, x := range suffix {
				if strings.HasSuffix(strings.ToLower(u.Path), x) {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
		return len(p.Segments) > 0
	}
	switch {
	case all(".aac"):
		return ".aac"
	case all(".mp3"):
		return ".mp3"
	case all(".mp4", ".m4s", ".cmfv"):
		return ".mp4"
	}
	return ".ts"
}

// validateTrack aplica as recusas por desenho a uma faixa já carregada.
func validateTrack(guard *urlGuard, media *Playlist, label string) error {
	switch {
	case media.KeyMethod != "":
		return unsupported("O stream está cifrado ("+media.KeyMethod+"). O StreamVault não descarrega streams cifrados.",
			"Se o conteúdo for seu, peça o ficheiro original a quem o publicou.")
	case !media.EndList:
		return unsupported("É uma transmissão ao vivo (a playlist não termina).", "Só vídeos em demanda (VOD) são suportados.")
	case media.ByteRange:
		return unsupported("O stream usa intervalos de bytes dentro de ficheiros (BYTERANGE).", "Ainda não suportado.")
	case len(media.Segments) == 0:
		return unsupported("A playlist"+label+" não tem segmentos.", "Confirme o link.")
	}
	for _, s := range media.Segments {
		if err := guard.check(s.URL); err != nil {
			return err
		}
	}
	if media.MapURL != "" {
		if err := guard.check(media.MapURL); err != nil {
			return err
		}
	}
	return nil
}

// isFMP4 diz se a faixa é MP4 fragmentado (a única que se sabe juntar sem recodificar).
func isFMP4(p *Playlist) bool {
	if p.Container != "" {
		return p.Container == "mp4"
	}
	return p.MapURL != ""
}

func (e *Engine) run(j *job) error {
	src := j.task.Source
	destDir := j.task.Policy.DestinationDir
	if destDir == "" {
		destDir = "."
	}
	guard := newGuard(src.URL)
	tracks, err := e.loadTracks(j, guard)
	if err != nil {
		return err
	}
	for i, tr := range tracks {
		label := ""
		if i > 0 {
			label = " de áudio"
		}
		if err := validateTrack(guard, tr.media, label); err != nil {
			return err
		}
	}
	multi := len(tracks) > 1
	if multi {
		for _, tr := range tracks {
			if !isFMP4(tr.media) {
				return unsupported("Este stream usa áudio em separado e só se juntam vídeo e áudio em MP4 fragmentado (este não é).",
					"Ainda não suportado para este formato (MPEG-TS/WebM).")
			}
		}
	}

	name := src.DisplayName
	if name == "" {
		name = path.Base(mustPath(tracks[0].url))
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".m3u8"), ".mpd")
	ext := outputExt(tracks[0].media)
	if multi {
		ext = ".mp4"
	}
	cleanName, findings := security.CheckFileName(name + ext)
	for _, f := range findings {
		if f.Severity == "block" {
			return domain.NewError(domain.ErrPolicyViolation, nil, f.Message, "Nome de ficheiro recusado.", domain.CertaintyObserved, false)
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		if os.IsPermission(err) {
			return domain.NewError(domain.ErrPermissionDenied, err, "Sem permissão para criar a pasta "+destDir,
				"Escolha outra pasta de destino.", domain.CertaintyObserved, false)
		}
		return domain.NewError(domain.ErrInvalidInput, err, "A pasta de destino não é válida: "+destDir,
			"Escolha outra pasta de destino.", domain.CertaintyObserved, false)
	}
	if !multi {
		return e.runSingle(j, guard, tracks[0].media, destDir, cleanName)
	}
	return e.runMulti(j, guard, tracks, destDir, cleanName)
}

// runSingle: uma só faixa, concatenada diretamente no ficheiro final.
func (e *Engine) runSingle(j *job, guard *urlGuard, media *Playlist, destDir, cleanName string) error {
	work := filepath.Join(destDir, ".svhls-"+safeID(j.id))
	outPath := filepath.Join(destDir, cleanName+".svpart")
	r, err := e.openTrack(j, guard, media, work, outPath, cleanName)
	if err != nil {
		return err
	}
	defer r.out.Close()
	if err := r.execute(); err != nil {
		return err
	}
	if j.isPaused() || j.isCancelled() {
		return nil
	}
	return r.finish(outPath, destDir, cleanName)
}

// runMulti: descarrega cada faixa para o seu ficheiro (com retoma própria) e junta-as
// num só MP4 sem recodificar (mp4merge). Uma faixa já concluída não se repete.
func (e *Engine) runMulti(j *job, guard *urlGuard, tracks []track, destDir, cleanName string) error {
	root := filepath.Join(destDir, ".svhls-"+safeID(j.id))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return diag.Explain(err, j.task.Source.Redacted())
	}
	var files []string
	var base int64
	for i, tr := range tracks {
		twork := filepath.Join(root, fmt.Sprintf("t%d", i))
		part := filepath.Join(root, fmt.Sprintf("track-%d.mp4", i))
		r, err := e.openTrack(j, guard, tr.media, twork, part, fmt.Sprintf("%s#t%d", cleanName, i))
		if err != nil {
			return err
		}
		r.base, r.unknownTotal = base, i < len(tracks)-1
		if !r.st.Done {
			if err := r.execute(); err != nil {
				r.out.Close()
				return err
			}
			if j.isPaused() || j.isCancelled() {
				r.out.Close()
				return nil
			}
			if err := r.out.Sync(); err != nil {
				r.out.Close()
				return diag.Explain(err, j.task.Source.Redacted())
			}
			r.mu.Lock()
			r.st.Done = true
			r.mu.Unlock()
			if err := r.st.save(twork); err != nil {
				r.out.Close()
				return diag.Explain(err, j.task.Source.Redacted())
			}
		}
		if err := r.out.Close(); err != nil {
			return diag.Explain(err, j.task.Source.Redacted())
		}
		base += r.st.AppendedBytes
		atomic.StoreInt64(&j.bytesDone, base)
		files = append(files, part)
	}

	merged := filepath.Join(destDir, cleanName+".svpart")
	_ = os.Remove(merged)
	if _, err := mp4merge.MergeFiles(merged, files); err != nil {
		_ = os.Remove(merged)
		return unsupported("Não foi possível juntar o vídeo e o áudio: "+err.Error(),
			"Este stream não é suportado.")
	}
	size, shaHex, err := hashFile(merged)
	if err != nil {
		return diag.Explain(err, j.task.Source.Redacted())
	}
	return j.complete(root, merged, destDir, cleanName, size, shaHex)
}

// openTrack prepara a descarga de UMA faixa: estado em disco, ficheiro parcial (com
// retoma até ao último ponto sincronizado) e hash. O chamador fecha r.out.
func (e *Engine) openTrack(j *job, guard *urlGuard, media *Playlist, work, outPath, key string) (*hlsRun, error) {
	src := j.task.Source
	destDir := filepath.Dir(outPath)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, diag.Explain(err, src.Redacted())
	}
	sum := sha256.New()
	psha := sha256.Sum256([]byte(fmt.Sprint(media.MapURL, "\n", segmentURLs(media))))
	st := loadState(work)
	if st == nil || st.PlaylistSHA != hex.EncodeToString(psha[:]) || st.N != len(media.Segments) || st.Name != key {
		// playlist mudou (ou 1.ª vez): recomeçar limpo
		clearDir(work)
		_ = os.Remove(outPath)
		st = &state{PlaylistSHA: hex.EncodeToString(psha[:]), N: len(media.Segments), Name: key}
	}
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if os.IsPermission(err) {
			return nil, domain.NewError(domain.ErrPermissionDenied, err, "Sem permissão para escrever em "+destDir,
				"Escolha outra pasta de destino.", domain.CertaintyObserved, false)
		}
		return nil, diag.Explain(err, src.Redacted())
	}
	fail := func(err error) (*hlsRun, error) { out.Close(); return nil, diag.Explain(err, src.Redacted()) }
	// retoma: o ficheiro de saída volta ao ponto sincronizado e o hash reconstrói-se
	if fi, serr := out.Stat(); serr != nil || fi.Size() < st.AppendedBytes {
		st.Appended, st.AppendedBytes, st.InitDone, st.Done = 0, 0, false, false
	}
	if err := out.Truncate(st.AppendedBytes); err != nil {
		return fail(err)
	}
	if st.AppendedBytes > 0 {
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return fail(err)
		}
		if _, err := io.CopyN(sum, out, st.AppendedBytes); err != nil {
			return fail(err)
		}
	}
	if _, err := out.Seek(st.AppendedBytes, io.SeekStart); err != nil {
		return fail(err)
	}
	// segmentos soltos de uma execução anterior não são de confiança (sem fsync): descartam-se
	clearSegments(work)
	return &hlsRun{e: e, j: j, media: media, work: work, out: out, st: st, sum: sum, guard: guard}, nil
}

// hashFile devolve o tamanho e o SHA-256 de um ficheiro.
func hashFile(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func mustPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "video"
	}
	return u.Path
}

func segmentURLs(p *Playlist) string {
	var b strings.Builder
	for _, s := range p.Segments {
		b.WriteString(s.URL)
		b.WriteByte('\n')
	}
	return b.String()
}

func clearDir(dir string) {
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func clearSegments(dir string) {
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "seg-") || e.Name() == "init.bin" || strings.HasSuffix(e.Name(), ".part") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// sharedLimiter limita a velocidade agregada de todos os pedidos.
type sharedLimiter struct {
	mu   sync.Mutex
	bps  float64
	next time.Time
}

func (l *sharedLimiter) wait(ctx context.Context, n int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.bps * float64(time.Second)))
	d := l.next.Sub(now) - 100*time.Millisecond
	l.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
}
