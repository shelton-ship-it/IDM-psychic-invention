// Package torrentengine implementa engine.Engine para magnet/.torrent (Fase 3)
// sobre uma interface Backend. Tudo o que é política do produto vive AQUI e
// é testável sem rede: validação do metainfo, correspondência com o info-hash
// do magnet, limites de tamanho, seleção de ficheiros, verificação por peças
// antes de "Completed" e pausa/retoma. O Backend concreto (anacrolix/torrent)
// só faz transporte P2P e é um adaptador fino (ver ADR-001).
package torrentengine

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"streamvault/core/diag"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/security"
	"streamvault/core/torrent"
)

// Source é o que o Backend recebe: um magnet OU o conteúdo de um .torrent.
type Source struct {
	Magnet   string
	InfoHash string // hex minúsculo, quando conhecido
	MetaInfo []byte // conteúdo do .torrent, quando fornecido
}

// Backend é o transporte P2P. Contrato de segurança: NÃO pode gravar bytes de
// conteúdo em disco antes de Session.Download — o motor valida primeiro o
// metainfo (caminhos, tamanho, info-hash).
type Backend interface {
	Add(ctx context.Context, src Source, destDir string) (Session, error)
	Close() error
}

// Session é uma transferência no Backend.
type Session interface {
	// Meta bloqueia até haver metainfo (.torrent bencode) ou ctx terminar.
	Meta(ctx context.Context) ([]byte, error)
	// Download inicia o conteúdo; selected==nil => todos os ficheiros.
	Download(selected map[int]bool) error
	// Progress devolve bytes concluídos e total dos ficheiros selecionados.
	Progress() (done, total int64)
	// Done fecha quando os ficheiros selecionados estão completos segundo o Backend.
	Done() <-chan struct{}
	// Stop interrompe a rede mantendo os dados em disco (pausa/cancelamento).
	Stop() error
}

// SelfVerifier é implementado opcionalmente por sessões cujo Backend verifica o
// SHA-1 de CADA peça antes de a gravar em disco. Serve para seleções parciais
// em que nenhuma peça fica inteira dentro dos ficheiros escolhidos (ex.: um
// ficheiro pequeno no meio do torrent): sem isto, o disco não consegue provar
// nada. Uma sessão que o declare sem o cumprir viola o contrato do Backend.
type SelfVerifier interface {
	VerifiesPieces() bool
}

// Options do motor.
type Options struct {
	Backend         Backend
	MetadataTimeout time.Duration // defeito 2 min
	ProgressEvery   time.Duration // defeito 500 ms
	// HTTPClient é usado só para obter .torrent por http(s). nil => defeito.
	HTTPClient *http.Client
	// AllowPrivateNetworks para obter .torrent de redes privadas (defeito: não).
	AllowPrivateNetworks bool
}

// Engine implementa engine.Engine.
type Engine struct {
	opts    Options
	mu      sync.Mutex
	handles map[string]*job
	closed  bool
}

// New cria o motor. Backend é obrigatório.
func New(o Options) (*Engine, error) {
	if o.Backend == nil {
		return nil, errors.New("torrentengine: Backend obrigatório")
	}
	if o.MetadataTimeout <= 0 {
		o.MetadataTimeout = 2 * time.Minute
	}
	if o.ProgressEvery <= 0 {
		o.ProgressEvery = 500 * time.Millisecond
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Engine{opts: o, handles: map[string]*job{}}, nil
}

func (e *Engine) Name() string { return "torrent" }

func (e *Engine) Capabilities() engine.Capabilities {
	return engine.Capabilities{SupportsPause: true, SupportsResume: true, SupportsMetadata: true,
		SupportsVerify: true, NeedsNetwork: true, EngineVersion: "1"}
}

func (e *Engine) CanHandle(src domain.DownloadSource) bool {
	return src.Kind == domain.SourceMagnet || src.Kind == domain.SourceTorrent
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
	return e.opts.Backend.Close()
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
}

func (j *job) TaskID() string { return j.id }
func (j *job) Pause(context.Context) error {
	j.mu.Lock()
	if j.cancelled {
		j.mu.Unlock()
		return errors.New("torrentengine: tarefa cancelada")
	}
	j.paused = true
	j.mu.Unlock()
	j.cancel()
	return nil
}
func (j *job) Resume(context.Context) error {
	return errors.New("torrentengine: use Orquestrador.Resume (novo Start; o Backend revalida os dados em disco)")
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
		BytesTotal: atomic.LoadInt64(&j.bytesTotal), UpdatedAt: time.Now().UTC()}
}
func (j *job) emit(kind engine.EventKind, payload map[string]string) {
	if j.sink != nil {
		j.sink.Emit(engine.Event{Kind: kind, TaskID: j.id, Progress: j.Snapshot(),
			Timestamp: time.Now().UTC().UnixMilli(), Payload: payload})
	}
}
func (j *job) emitErr(de *domain.DownloadError) {
	if j.sink != nil {
		j.sink.Emit(engine.Event{Kind: engine.EventError, TaskID: j.id, Progress: j.Snapshot(),
			Error: de, Timestamp: time.Now().UTC().UnixMilli()})
	}
}

// Start arranca a tarefa em goroutine própria.
func (e *Engine) Start(ctx context.Context, req engine.StartRequest) (engine.JobHandle, error) {
	if !e.CanHandle(req.Task.Source) {
		return nil, fmt.Errorf("torrentengine: origem %q não suportada", req.Task.Source.Kind)
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("torrentengine: motor fechado")
	}
	if prev, dup := e.handles[req.Task.ID]; dup {
		if !prev.isPaused() {
			e.mu.Unlock()
			return nil, fmt.Errorf("torrentengine: tarefa %s já ativa", req.Task.ID)
		}
		e.mu.Unlock()
		select {
		case <-prev.done:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("torrentengine: tarefa %s ainda a pausar", req.Task.ID)
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return nil, errors.New("torrentengine: motor fechado")
		}
		if _, dup2 := e.handles[req.Task.ID]; dup2 {
			e.mu.Unlock()
			return nil, fmt.Errorf("torrentengine: tarefa %s já ativa", req.Task.ID)
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
		if j.err != nil && !j.isPaused() && !j.isCancelled() {
			if de, ok := j.err.(*domain.DownloadError); ok {
				j.emitErr(de)
			} else {
				j.emitErr(diag.Explain(j.err, j.task.Source.Redacted()))
			}
		}
	}()
	return j, nil
}

// loadSource obtém o Source para o Backend (lê/baixa o .torrent se for o caso).
func (e *Engine) loadSource(j *job) (Source, *domain.DownloadError) {
	src := j.task.Source
	switch src.Kind {
	case domain.SourceMagnet:
		h, err := normalizeInfoHash(src.InfoHash)
		if err != nil {
			return Source{}, domain.NewError(domain.ErrInvalidInput, err, "info-hash do magnet inválido.",
				"Confirme o link magnet.", domain.CertaintyObserved, false)
		}
		return Source{Magnet: src.URL, InfoHash: h}, nil
	case domain.SourceTorrent:
		data, de := e.readTorrentFile(j.ctx, src.URL)
		if de != nil {
			return Source{}, de
		}
		return Source{MetaInfo: data}, nil
	}
	return Source{}, domain.NewError(domain.ErrUnsupportedSource, nil, "origem não suportada pelo motor torrent.",
		"Use magnet ou .torrent.", domain.CertaintyObserved, false)
}

func (e *Engine) readTorrentFile(ctx context.Context, raw string) ([]byte, *domain.DownloadError) {
	bad := func(msg string, err error) *domain.DownloadError {
		return domain.NewError(domain.ErrInvalidInput, err, msg, "Confirme o ficheiro .torrent.", domain.CertaintyObserved, false)
	}
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "file" {
		fi, serr := os.Stat(u.Path)
		if serr != nil {
			return nil, bad("ficheiro .torrent inacessível.", serr)
		}
		if fi.Size() > torrent.MaxMetaSize {
			return nil, bad("ficheiro .torrent demasiado grande.", nil)
		}
		data, rerr := os.ReadFile(u.Path)
		if rerr != nil {
			return nil, bad("não foi possível ler o .torrent.", rerr)
		}
		return data, nil
	}
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, bad("origem .torrent não suportada (use file:// ou http(s)).", err)
	}
	sopts := security.DefaultOptions()
	sopts.AllowPrivateNetworks = e.opts.AllowPrivateNetworks
	for _, f := range security.CheckURL(raw, sopts) {
		if f.Severity == "block" {
			return nil, domain.NewError(domain.ErrPolicyViolation, nil, f.Message,
				"Use uma origem pública.", domain.CertaintyObserved, false)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	resp, err := e.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, diag.Explain(err, "torrent")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, domain.NewError(domain.ErrHTTPStatus, nil, fmt.Sprintf("HTTP %d ao obter o .torrent.", resp.StatusCode),
			"Confirme o link.", domain.CertaintyObserved, true)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, torrent.MaxMetaSize+1))
	if err != nil {
		return nil, diag.Explain(err, "torrent")
	}
	if len(data) > torrent.MaxMetaSize {
		return nil, bad(".torrent excede o tamanho máximo.", nil)
	}
	return data, nil
}

// run é o ciclo completo de uma transferência.
func (e *Engine) run(j *job) error {
	policy := j.task.Policy
	destDir := policy.DestinationDir
	if destDir == "" {
		destDir = "."
	}
	src, de := e.loadSource(j)
	if de != nil {
		return de
	}
	// Se já temos o .torrent, valida-o ANTES de o dar ao Backend.
	var meta *torrent.Meta
	if len(src.MetaInfo) > 0 {
		m, err := torrent.Parse(src.MetaInfo)
		if err != nil {
			return domain.NewError(domain.ErrInvalidInput, err, "ficheiro .torrent inválido ou inseguro: "+err.Error(),
				"Obtenha outro .torrent.", domain.CertaintyObserved, false)
		}
		meta = m
	}
	sess, err := e.opts.Backend.Add(j.ctx, src, destDir)
	if err != nil {
		return diag.Explain(err, j.task.Source.Redacted())
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = sess.Stop()
		}
	}
	defer stop()

	// 1. Metadados (separados do conteúdo; nada é gravado ainda).
	if meta == nil {
		mctx, cancel := context.WithTimeout(j.ctx, e.opts.MetadataTimeout)
		raw, merr := sess.Meta(mctx)
		cancel()
		if merr != nil {
			if j.isPaused() || j.isCancelled() {
				return nil
			}
			if errors.Is(merr, context.DeadlineExceeded) {
				return domain.NewError(domain.ErrConnectionLost, merr,
					"Não foi possível obter os metadados do torrent (sem peers/DHT a responder).",
					"Tente mais tarde ou use um .torrent.", domain.CertaintyHeuristic, true)
			}
			return diag.Explain(merr, j.task.Source.Redacted())
		}
		m, perr := torrent.Parse(raw)
		if perr != nil {
			return domain.NewError(domain.ErrPolicyViolation, perr,
				"metadados recebidos são inválidos ou inseguros: "+perr.Error(),
				"Torrent rejeitado por segurança.", domain.CertaintyObserved, false)
		}
		meta = m
	}
	// 2. O metainfo TEM de corresponder ao info-hash pedido (magnet ou tarefa).
	if want := j.task.Source.InfoHash; want != "" {
		if h, herr := normalizeInfoHash(want); herr == nil && h != meta.InfoHash {
			return domain.NewError(domain.ErrIntegrityFailure, nil,
				"os metadados recebidos não correspondem ao info-hash do magnet.",
				"Possível adulteração; torrent rejeitado.", domain.CertaintyObserved, false)
		}
	}
	// 3. Seleção de ficheiros + limites.
	selected, de := selectFiles(meta, j.task.Files)
	if de != nil {
		return de
	}
	var selTotal int64
	for _, f := range meta.Files {
		if selected == nil || selected[f.Index] {
			selTotal += f.Length
		}
	}
	atomic.StoreInt64(&j.bytesTotal, selTotal)
	if policy.MaxSizeBytes > 0 && selTotal > policy.MaxSizeBytes {
		return domain.NewError(domain.ErrPolicyViolation, nil,
			fmt.Sprintf("O torrent tem %d bytes; limite configurado é %d.", selTotal, policy.MaxSizeBytes),
			"Aumente o limite ou selecione menos ficheiros.", domain.CertaintyObserved, false)
	}
	j.emit(engine.EventFileDiscovered, map[string]string{"name": meta.Name, "size": fmt.Sprint(selTotal),
		"files": fmt.Sprint(len(meta.Files)), "info_hash": meta.InfoHash})

	// 4. Conteúdo.
	if err := sess.Download(selected); err != nil {
		return diag.Explain(err, j.task.Source.Redacted())
	}
	tick := time.NewTicker(e.opts.ProgressEvery)
	defer tick.Stop()
wait:
	for {
		select {
		case <-j.ctx.Done():
			stop()
			return nil // pausa/cancelamento: saída controlada (job.err == nil)
		case <-tick.C:
			d, _ := sess.Progress()
			atomic.StoreInt64(&j.bytesDone, d)
			j.emit(engine.EventProgress, nil)
		case <-sess.Done():
			break wait
		}
	}
	d, _ := sess.Progress()
	atomic.StoreInt64(&j.bytesDone, d)
	stop() // deixa de semear/consumir rede antes de verificar

	// 5. Verificação por peças — SEM isto nunca se emite Completed.
	bad, verified, verr := torrent.VerifyPieces(destDir, meta, selected)
	if verr != nil {
		return domain.NewError(domain.ErrCorruptData, verr, "falha ao verificar os dados no disco: "+verr.Error(),
			"Verifique o disco e retome.", domain.CertaintyObserved, true)
	}
	if len(bad) > 0 {
		j.emit(engine.EventIntegrityReport, map[string]string{"result": "mismatch", "bad_pieces": fmt.Sprint(len(bad))})
		return domain.NewError(domain.ErrIntegrityFailure, nil,
			fmt.Sprintf("%d peça(s) falharam a verificação SHA-1 do torrent.", len(bad)),
			"Retome para voltar a descarregar as peças inválidas.", domain.CertaintyObserved, true)
	}
	proof := "disk"
	if verified == 0 && selTotal > 0 {
		if sv, ok := sess.(SelfVerifier); !ok || !sv.VerifiesPieces() {
			return domain.NewError(domain.ErrIntegrityFailure, nil,
				"nenhuma peça pôde ser verificada para a seleção atual.", "Selecione ficheiros completos.",
				domain.CertaintyObserved, false)
		}
		proof = "backend" // cada peça foi verificada (SHA-1) pelo Backend antes de ser gravada
	}
	j.emit(engine.EventIntegrityReport, map[string]string{"result": "ok", "pieces_verified": fmt.Sprint(verified), "proof": proof})
	j.emit(engine.EventCompleted, map[string]string{"path": meta.ContentRoot(destDir),
		"verified": "pieces", "info_hash": meta.InfoHash})
	return nil
}

// selectFiles devolve o conjunto selecionado (nil => todos). Índices fora do
// intervalo são recusados — nunca ignorados em silêncio.
func selectFiles(m *torrent.Meta, files []domain.FileMetadata) (map[int]bool, *domain.DownloadError) {
	any := false
	for _, f := range files {
		if f.Selected {
			any = true
		}
	}
	if !any {
		return nil, nil
	}
	sel := map[int]bool{}
	for _, f := range files {
		if !f.Selected {
			continue
		}
		if f.Index < 0 || f.Index >= len(m.Files) {
			return nil, domain.NewError(domain.ErrInvalidInput, nil,
				fmt.Sprintf("ficheiro selecionado %d não existe (o torrent tem %d).", f.Index, len(m.Files)),
				"Reabra a seleção de ficheiros.", domain.CertaintyObserved, false)
		}
		sel[f.Index] = true
	}
	return sel, nil
}

// normalizeInfoHash converte hex(40) ou base32(32) em hex minúsculo.
func normalizeInfoHash(h string) (string, error) {
	h = strings.TrimSpace(h)
	switch len(h) {
	case 40:
		if _, err := hex.DecodeString(h); err != nil {
			return "", err
		}
		return strings.ToLower(h), nil
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
		if err != nil || len(b) != 20 {
			return "", fmt.Errorf("base32 inválido")
		}
		return hex.EncodeToString(b), nil
	}
	return "", fmt.Errorf("comprimento %d inválido", len(h))
}
