// Package orchestrator coordena motores através das interfaces engine.Engine.
// A fila NÃO conhece HTTP nem BitTorrent — apenas contratos. Isto satisfaz o
// requisito "substituir motor sem reescrever fila/licenciamento/UI".
package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"streamvault/core/diag"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/integrity"
)

// Store persiste tarefas (implementado em storage/ na Fase 4; interface aqui
// para manter o orquestrador testável sem disco).
type Store interface {
	Save(t *domain.DownloadTask) error
	Get(id string) (*domain.DownloadTask, error)
	List() ([]*domain.DownloadTask, error)
	Delete(id string) error
}

// EngineRouter escolhe o motor para cada origem via CanHandle.
type EngineRouter struct {
	mu     sync.RWMutex
	engine map[domain.SourceKind]engine.Engine
}

func NewEngineRouter() *EngineRouter {
	return &EngineRouter{engine: map[domain.SourceKind]engine.Engine{}}
}

func (r *EngineRouter) Register(kind domain.SourceKind, e engine.Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engine[kind] = e
}

func (r *EngineRouter) For(src domain.DownloadSource) (engine.Engine, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.engine[src.Kind]
	if !ok || !e.CanHandle(src) {
		return nil, domain.NewError(domain.ErrUnsupportedSource, nil,
			fmt.Sprintf("nenhum motor registrado trata origem %q", src.Kind),
			"Atualize a aplicação ou use uma origem suportada.", domain.CertaintyObserved, false)
	}
	return e, nil
}

// Orchestrator é o coração da fila: prioridades, concorrência, controlo.
type Orchestrator struct {
	mu            sync.Mutex
	store         Store
	router        *EngineRouter
	sink          engine.EventSink
	handles       map[string]engine.JobHandle
	maxConc       int
	running       int
	ctx           context.Context
	cancel        context.CancelFunc
	clock         func() time.Time
	seq           uint64 // sequência p/ IDs únicos mesmo em chamadas no mesmo ns
	onEngineEvent func(engine.Event)
}

type Options struct {
	Store     Store
	Router    *EngineRouter
	Sink      engine.EventSink
	MaxConcur int
	Clock     func() time.Time // injetável p/ testes
	// OnEngineEvent: motores emitem eventos (progresso/conclusão/falha) pelo
	// Sink; o orquestrador precisa de os observar para libertar capacidade e
	// persistir estados terminais. Um Fanout multiplexa para vários consumidores.
	OnEngineEvent func(engine.Event)
}

// Fanout permite que UI e orquestrador consumam o mesmo EventSink sem bloqueio.
type Fanout struct {
	mu   sync.RWMutex
	subs map[int]func(engine.Event)
	next int
	buf  chan engine.Event
	done chan struct{}
}

func NewFanout(buffer int) *Fanout {
	if buffer <= 0 {
		buffer = 256
	}
	f := &Fanout{buf: make(chan engine.Event, buffer), done: make(chan struct{}), subs: map[int]func(engine.Event){}}
	go f.run()
	return f
}

// Subscribe regista um consumidor e devolve a função que o cancela (clientes
// SSE/UI que se ligam e desligam não podem acumular subscrições).
func (f *Fanout) Subscribe(fn func(engine.Event)) (cancel func()) {
	f.mu.Lock()
	id := f.next
	f.next++
	f.subs[id] = fn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.subs, id)
		f.mu.Unlock()
	}
}

func (f *Fanout) Emit(e engine.Event) {
	select {
	case f.buf <- e:
	default:
		// Sink não pode bloquear o caminho de dados (contrato engine.EventSink):
		// sobressaturação descarta o evento MAIS ANTIGO para dar prioridade ao
		// estado atual — progresso é sempre re-emitido; estados terminais são
		// re-sincronizáveis via store na próxima List().
		select {
		case <-f.buf:
		default:
		}
		select {
		case f.buf <- e:
		default:
		}
	}
}

func (f *Fanout) run() {
	for {
		select {
		case e := <-f.buf:
			f.mu.RLock()
			for _, s := range f.subs {
				s(e)
			}
			f.mu.RUnlock()
		case <-f.done:
			return
		}
	}
}

func (f *Fanout) Close() { close(f.done) }

func New(o Options) (*Orchestrator, error) {
	if o.Store == nil || o.Router == nil {
		return nil, fmt.Errorf("orchestrator: Store e Router obrigatórios")
	}
	if o.MaxConcur <= 0 {
		o.MaxConcur = 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	or := &Orchestrator{
		store: o.Store, router: o.Router, sink: o.Sink,
		maxConc: o.MaxConcur, handles: map[string]engine.JobHandle{},
		ctx: ctx, cancel: cancel, clock: clock,
	}
	// Observar eventos dos motores para libertar capacidade e persistir
	// estados terminais (completed/failed/cancelled) reportados pelo motor.
	if o.OnEngineEvent != nil {
		or.onEngineEvent = o.OnEngineEvent
	}
	return or, nil
}

// onEngineEvent é chamado (via Fanout pelo chamador) para cada evento de motor.
// O orquestrador reage apenas a estados terminais — progresso pertence ao UI.
func (or *Orchestrator) SetEngineObserver(fn func(engine.Event)) {
	or.mu.Lock()
	or.onEngineEvent = fn
	or.mu.Unlock()
}

// ObserveEngine trata um evento vindo de um motor. Seguro para ser ligado a
// Fanout.Subscribe. Retorna true se o evento alterou capacidade ativa.
func (or *Orchestrator) ObserveEngine(e engine.Event) bool {
	switch e.Kind {
	case engine.EventCompleted, engine.EventError:
	default:
		return false
	}
	or.mu.Lock()
	_, active := or.handles[e.TaskID]
	if active {
		delete(or.handles, e.TaskID)
		if or.running > 0 {
			or.running--
		}
	}
	or.mu.Unlock()
	if !active {
		return false // já tratado (ex.: cancel/pausa explícitos)
	}
	t, err := or.store.Get(e.TaskID)
	if err != nil {
		return true
	}
	switch e.Kind {
	case engine.EventError:
		if e.Error != nil && (t.State == domain.StateRunning || t.State == domain.StateVerifying) {
			t.FailRegister(e.Error)
		}
	case engine.EventCompleted:
		or.finalize(t, e)
	}
	or.store.Save(t)
	return true
}

// finalize leva uma tarefa Running a Completed PASSANDO por Verifying e só
// conclui se a política de integridade for provada. O motor afirma "terminei";
// quem decide "completed" é o orquestrador, com evidência (nunca às cegas).
func (or *Orchestrator) finalize(t *domain.DownloadTask, e engine.Event) {
	if t.State != domain.StateRunning {
		return
	}
	if err := t.Transition(domain.StateVerifying); err != nil {
		return
	}
	or.emitState(t)
	if de := verifyCompletion(t, e); de != nil {
		t.FailRegister(de)
		return
	}
	if e.Progress.TaskID != "" {
		t.Progress = e.Progress
	}
	recordFile(t, e)
	if err := t.Transition(domain.StateCompleted); err != nil {
		t.FailRegister(domain.NewError(domain.ErrIntegrityFailure, err, err.Error(),
			"Reinicie a transferência.", domain.CertaintyObserved, true))
		return
	}
	t.Progress.State = domain.StateCompleted
	or.emitState(t)
}

// recordFile guarda na tarefa o ficheiro final (nome real, tamanho, hash) para
// que organização, duplicados e UI saibam ONDE ficou o resultado. Um hash
// esperado fornecido pela origem nunca é substituído.
func recordFile(t *domain.DownloadTask, e engine.Event) {
	path := e.Payload["path"]
	if path == "" {
		return
	}
	var size int64
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	sum := strings.ToLower(e.Payload["sha256"])
	for i := range t.Files {
		if t.Files[i].Index == 0 {
			t.Files[i].Path, t.Files[i].SizeBytes = filepath.Base(path), size
			if t.Files[i].SHA256Hex == "" {
				t.Files[i].SHA256Hex = sum
			}
			return
		}
	}
	t.Files = append(t.Files, domain.FileMetadata{Index: 0, Path: filepath.Base(path),
		SizeBytes: size, SHA256Hex: sum, Selected: true})
}

// verifyCompletion aplica a política de integridade à conclusão reportada.
func verifyCompletion(t *domain.DownloadTask, e engine.Event) *domain.DownloadError {
	fail := func(msg, action string, retry bool) *domain.DownloadError {
		return domain.NewError(domain.ErrIntegrityFailure, nil, msg, action, domain.CertaintyObserved, retry)
	}
	path := e.Payload["path"]
	switch t.Policy.RequireIntegrity {
	case domain.IntegrityNone:
		return nil
	case domain.IntegritySHA256:
		if path == "" {
			return fail("O motor concluiu sem indicar o ficheiro; não é possível verificar o SHA-256.",
				"Reinicie a transferência.", true)
		}
		expected := ""
		for _, f := range t.Files {
			if f.Index == 0 {
				expected = f.SHA256Hex
			}
		}
		res, _, err := integrity.VerifyFile(path, expected)
		switch {
		case err != nil:
			return fail("Falha ao ler o ficheiro para verificação: "+err.Error(), "Verifique o disco e retome.", true)
		case res == integrity.ResultOK:
			return nil
		case res == integrity.ResultMissingExpected:
			return fail("A política exige SHA-256 mas a origem não forneceu hash.",
				"Forneça o hash esperado ou relaxe a política conscientemente.", false)
		default:
			return fail("Verificação SHA-256 falhou ("+res.String()+").",
				"Apague o ficheiro e transfira de novo.", true)
		}
	case domain.IntegrityRemoteETag:
		if path == "" {
			return fail("O motor concluiu sem indicar o ficheiro; não é possível validar o tamanho.",
				"Reinicie a transferência.", true)
		}
		st, err := os.Stat(path)
		if err != nil {
			return fail("Ficheiro final inacessível: "+err.Error(), "Retome a transferência.", true)
		}
		if tot := e.Progress.BytesTotal; tot > 0 && st.Size() != tot {
			return fail(fmt.Sprintf("Tamanho final %d difere do remoto %d.", st.Size(), tot),
				"Retome a transferência.", true)
		}
		return nil
	case domain.IntegrityTorrentPieces:
		if e.Payload["verified"] != "pieces" {
			return fail("O motor não confirmou a verificação por peças.", "Reinicie a transferência.", true)
		}
		return nil
	}
	return fail("Nível de integridade desconhecido.", "Atualize a aplicação.", false)
}

// Submit cria tarefa a partir de origem reconhecida e enfileira.
func (o *Orchestrator) Submit(src domain.DownloadSource, policy domain.DownloadPolicy) (*domain.DownloadTask, error) {
	now := o.clock().UTC()
	o.mu.Lock()
	o.seq++
	n := o.seq
	o.mu.Unlock()
	t := &domain.DownloadTask{
		ID: newTaskID(now, n), Source: src, Policy: policy,
		State: domain.StatePending, CreatedAt: now, UpdatedAt: now,
		Progress: domain.ProgressSnapshot{BytesTotal: -1},
	}
	if err := o.store.Save(t); err != nil {
		return nil, fmt.Errorf("orchestrator: persistir: %w", err)
	}
	return t, nil
}

// SetPriority reordena uma tarefa pendente/falhada (maior = primeiro).
// Tarefas ativas não podem ser reordenadas em voo — isso exigiria reiniciar;
// o briefing pede reordenação, não reordenação sem custo, logo recusamos
// silenciosamente mudanças em running para manter honestidade do estado.
func (o *Orchestrator) SetPriority(id string, priority int) error {
	t, err := o.store.Get(id)
	if err != nil {
		return err
	}
	if t.State != domain.StatePending && t.State != domain.StateFailed {
		return fmt.Errorf("orchestrator: só tarefas pendentes/falhadas podem ser reordenadas (%s está %s)", id, t.State)
	}
	t.Priority = priority
	t.UpdatedAt = o.clock().UTC()
	return o.store.Save(t)
}

// StartNext inicia a próxima tarefa elegível (prioridade maior primeiro).
func (o *Orchestrator) StartNext() (*domain.DownloadTask, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running >= o.maxConc {
		return nil, nil // sem capacidade; chamador agenda retry
	}
	tasks, err := o.store.List()
	if err != nil {
		return nil, err
	}
	var pending []*domain.DownloadTask
	for _, t := range tasks {
		if t.State == domain.StatePending {
			pending = append(pending, t)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].Priority != pending[j].Priority {
			return pending[i].Priority > pending[j].Priority
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	if len(pending) == 0 {
		return nil, nil
	}
	t := pending[0]
	eng, err := o.router.For(t.Source)
	if err != nil {
		de := domain.NewError(domain.ErrUnsupportedSource, err, err.Error(),
			"Confirme o link; se a origem é válida mas não suportada, aguarde uma atualização com novos adaptadores.",
			domain.CertaintyObserved, false)
		t.FailRegister(de)
		o.store.Save(t)
		return t, err
	}
	if err := t.Transition(domain.StateRunning); err != nil {
		return nil, err
	}
	h, err := eng.Start(o.ctx, engine.StartRequest{Task: *t, Sink: o.sink})
	if err != nil {
		t.FailRegister(diag.Explain(err, t.Source.Redacted()))
		o.store.Save(t)
		return t, err
	}
	o.handles[t.ID] = h
	o.running++
	o.store.Save(t)
	o.emitState(t)
	return t, nil
}

// Pause pede ao motor para parar mantendo estado recuperável e LIBERTA o slot
// de concorrência (uma tarefa pausada não ocupa capacidade).
func (o *Orchestrator) Pause(id string) error {
	o.mu.Lock()
	h, ok := o.handles[id]
	o.mu.Unlock()
	if !ok {
		return fmt.Errorf("orchestrator: tarefa %s não está ativa", id)
	}
	if err := h.Pause(o.ctx); err != nil {
		return err
	}
	o.releaseSlot(id)
	return o.transitionAndSave(id, domain.StatePaused)
}

// Resume reinicia a tarefa pausada com um NOVO Start no motor (o motor retoma
// a partir do estado persistido). Respeita o limite de concorrência.
func (o *Orchestrator) Resume(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, active := o.handles[id]; active {
		return fmt.Errorf("orchestrator: tarefa %s já está ativa", id)
	}
	t, err := o.store.Get(id)
	if err != nil {
		return err
	}
	if t.State != domain.StatePaused {
		return fmt.Errorf("orchestrator: só tarefas pausadas retomam (%s está %s)", id, t.State)
	}
	if o.running >= o.maxConc {
		return fmt.Errorf("orchestrator: sem capacidade para retomar %s (limite %d)", id, o.maxConc)
	}
	eng, err := o.router.For(t.Source)
	if err != nil {
		return err
	}
	if err := t.Transition(domain.StateRunning); err != nil {
		return err
	}
	h, err := eng.Start(o.ctx, engine.StartRequest{Task: *t, Sink: o.sink})
	if err != nil {
		t.State = domain.StatePaused // Start não ocorreu: mantém-se pausada
		return err
	}
	o.handles[id] = h
	o.running++
	o.store.Save(t)
	o.emitState(t)
	return nil
}

// Cancel é terminal. Funciona também em tarefas pendentes, pausadas ou
// falhadas (sem handle ativo): só o motor é contactado se houver handle.
func (o *Orchestrator) Cancel(id string) error {
	o.mu.Lock()
	h, ok := o.handles[id]
	o.mu.Unlock()
	if ok {
		if err := h.Cancel(o.ctx); err != nil {
			return err
		}
		o.releaseSlot(id)
	}
	return o.transitionAndSave(id, domain.StateCancelled)
}

func (o *Orchestrator) releaseSlot(id string) {
	o.mu.Lock()
	if _, still := o.handles[id]; still {
		delete(o.handles, id)
		if o.running > 0 {
			o.running--
		}
	}
	o.mu.Unlock()
}

func (o *Orchestrator) transitionAndSave(id string, target domain.State) error {
	t, err := o.store.Get(id)
	if err != nil {
		return err
	}
	if err := t.Transition(target); err != nil {
		return err
	}
	if err := o.store.Save(t); err != nil {
		return err
	}
	o.emitState(t)
	return nil
}

// Recover marca Running->Failed após deteção de encerramento inesperado
// (chamado no arranque com estado carregado do store): nenhuma transferência
// sobreviveu ao processo, logo nada pode continuar "a correr" fantasma.
func (o *Orchestrator) Recover() (int, error) {
	tasks, err := o.store.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tasks {
		if t.State == domain.StateRunning || t.State == domain.StateVerifying {
			t.FailRegister(domain.NewError(domain.ErrConnectionLost, nil,
				"Transferência interrompida por fecho da aplicação.",
				"Pode retomar; o progresso parcial em disco foi preservado.",
				domain.CertaintyHeuristic, true))
			// Failed->Pending é transição válida (retry explícito)
			if err := t.Transition(domain.StatePending); err == nil {
				o.store.Save(t)
				n++
			}
		}
	}
	return n, nil
}

func (o *Orchestrator) emitState(t *domain.DownloadTask) {
	if o.sink == nil {
		return
	}
	o.sink.Emit(engine.Event{
		Kind: engine.EventStateChanged, TaskID: t.ID, State: t.State,
		Progress: t.Progress, Timestamp: o.clock().UnixMilli(),
	})
}

func (o *Orchestrator) Shutdown(ctx context.Context) error {
	o.cancel()
	return nil
}

func newTaskID(now time.Time, seq uint64) string {
	return fmt.Sprintf("dl-%d-%06d", now.UnixNano()%1e15, seq%1000000)
}
