package httpengine

// Download acelerado por várias ligações (como o IDM/1DM).
//
// O ficheiro é dividido em segmentos pequenos servidos a N trabalhadores por
// uma fila (quem acaba primeiro puxa o seguinte: balanceamento dinâmico).
// Cada segmento é pedido com "Range" e escrito com WriteAt no ficheiro
// temporário pré-alocado. O estado de retoma é POR SEGMENTO (bytes feitos de
// cada um), gravado só depois de um fsync, e a divisão recalcula-se de
// (Total, SegSize). Segurança de dados:
//   - If-Range: se o recurso mudou, o servidor devolve 200 e abortamos (nunca
//     juntar bytes de versões diferentes);
//   - cada 206 é validado contra o intervalo e o total pedidos;
//   - servidores que ignoram Range (200) levam a fallback limpo para uma
//     ligação; servidores que limitam ligações (429/503) reduzem a concorrência.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"streamvault/core/diag"
	"streamvault/core/domain"
	"streamvault/core/engine"
)

const (
	defaultConnections = 8
	maxConnections     = 32
	defaultMinSegment  = 1 << 20
	maxSegmentSize     = 32 << 20
	persistInterval    = 2 * time.Second
)

// ---- estado por segmento ----

type segState struct {
	mu    sync.Mutex
	size  int64
	total int64
	done  []int64
}

func newSegState(total, size int64) *segState {
	n := (total + size - 1) / size
	return &segState{size: size, total: total, done: make([]int64, n)}
}

func (s *segState) count() int { return len(s.done) }

// bounds devolve [start,end] (end inclusivo) do segmento i.
func (s *segState) bounds(i int) (int64, int64) {
	start := int64(i) * s.size
	end := start + s.size - 1
	if end > s.total-1 {
		end = s.total - 1
	}
	return start, end
}

func (s *segState) length(i int) int64 { a, b := s.bounds(i); return b - a + 1 }

func (s *segState) doneOf(i int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done[i]
}

func (s *segState) add(i int, n int64) {
	s.mu.Lock()
	s.done[i] += n
	s.mu.Unlock()
}

func (s *segState) sum() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t int64
	for _, d := range s.done {
		t += d
	}
	return t
}

func (s *segState) snapshot() (int64, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size, append([]int64(nil), s.done...)
}

// segResumeValid confere que o estado persistido é coerente com (Total, SegSize).
func segResumeValid(rs *ResumeState) bool {
	if rs.SegSize <= 0 || rs.Total <= 0 {
		return false
	}
	st := &segState{size: rs.SegSize, total: rs.Total, done: make([]int64, len(rs.SegDone))}
	if int64(len(rs.SegDone)) != (rs.Total+rs.SegSize-1)/rs.SegSize {
		return false
	}
	for i, d := range rs.SegDone {
		if d < 0 || d > st.length(i) {
			return false
		}
	}
	return true
}

func segStateFromResume(rs *ResumeState) *segState {
	return &segState{size: rs.SegSize, total: rs.Total, done: append([]int64(nil), rs.SegDone...)}
}

// planSegSize: ~4 segmentos por ligação (para balancear), entre minSeg e 32 MiB.
func planSegSize(total int64, conns int, minSeg int64) int64 {
	s := total / int64(conns*4)
	if s < minSeg {
		s = minSeg
	}
	if s > maxSegmentSize {
		s = maxSegmentSize
	}
	return s
}

// ---- parâmetros por defeito ----

func (e *Engine) minSegment() int64 {
	if e.opts.MinSegmentBytes > 0 {
		return e.opts.MinSegmentBytes
	}
	return defaultMinSegment
}

func (e *Engine) connectionsFor(j *job) int {
	c := e.opts.Connections
	if c <= 0 {
		c = defaultConnections
	}
	if p := j.task.Policy.MaxConcurrency; p > 0 {
		c = p
	}
	if c > maxConnections {
		c = maxConnections
	}
	if c < 1 {
		c = 1
	}
	return c
}

func (e *Engine) stallTimeout() time.Duration {
	if e.opts.SegmentStall > 0 {
		return e.opts.SegmentStall
	}
	return 30 * time.Second
}

func (e *Engine) retries() int {
	if e.opts.SegmentRetries > 0 {
		return e.opts.SegmentRetries
	}
	return 5
}

func (e *Engine) backoff(attempt int) time.Duration {
	base := e.opts.SegmentBackoff
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	d := base << uint(attempt-1)
	if d > 8*time.Second || d <= 0 {
		d = 8 * time.Second
	}
	return d
}

// ---- planeamento ----

type segParams struct {
	resp                    *http.Response // 200 da 1.ª ligação, reutilizada no segmento 0
	total, segSize          int64
	conns                   int
	resume                  *ResumeState
	tmp, destDir, cleanName string
	etag, lastMod           string
	src                     domain.DownloadSource
}

// planSegments decide se o download pode ser segmentado e como.
func (e *Engine) planSegments(j *job, resp *http.Response, total int64, probe bool, resume *ResumeState) (segParams, bool) {
	if probe {
		n := int((resume.Total + resume.SegSize - 1) / resume.SegSize)
		c := e.connectionsFor(j)
		if c > n {
			c = n
		}
		return segParams{total: resume.Total, segSize: resume.SegSize, conns: c, resume: resume}, true
	}
	if total < 0 || detectRangeSupport(resp.Header) != rangeSupported {
		return segParams{}, false
	}
	if ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce != "" && ce != "identity" {
		return segParams{}, false // corpo comprimido: o tamanho não é o do ficheiro
	}
	conns := e.connectionsFor(j)
	minSeg := e.minSegment()
	if int64(conns) > total/minSeg {
		conns = int(total / minSeg)
	}
	if conns < 2 {
		return segParams{}, false
	}
	return segParams{total: total, segSize: planSegSize(total, conns, minSeg), conns: conns}, true
}

func (e *Engine) abandonSegmented(j *job, resume *ResumeState) error {
	if resume != nil && resume.TempPath != "" {
		_ = os.Remove(resume.TempPath)
	}
	j.clearResume()
	return errFallbackSingle
}

func (j *job) clearResume() {
	j.seg.Store(nil)
	if j.opts_persist != nil {
		j.opts_persist(ResumeState{TaskID: j.id}) // BytesDone 0 => "sem retoma"
	}
}

// ---- limitador de velocidade partilhado (agregado de todas as ligações) ----

type sharedLimiter struct {
	mu   sync.Mutex
	bps  float64
	next time.Time
}

func newSharedLimiter(bps int64) *sharedLimiter {
	if bps <= 0 {
		return nil
	}
	return &sharedLimiter{bps: float64(bps)}
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
	d := l.next.Sub(now) - 100*time.Millisecond // pequena rajada tolerada
	l.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
}

// ---- erros internos ----

type rateLimitErr struct{ retryAfter time.Duration }

func (r *rateLimitErr) Error() string { return "servidor limitou as ligações (429/503)" }

var errStalled = errors.New("sem dados dentro do tempo limite")

// errRetire: este trabalhador já não é necessário (o servidor aceita menos ligações).
var errRetire = errors.New("trabalhador reformado")

// ---- execução ----

type segRun struct {
	e      *Engine
	j      *job
	p      segParams
	f      *os.File
	seg    *segState
	lim    *sharedLimiter
	ctx    context.Context
	rawURL string
	active int32 // ligações com corpo a ser lido neste momento
}

func (e *Engine) downloadSegmented(j *job, p segParams) error {
	src := p.src
	flags := os.O_CREATE | os.O_RDWR
	if p.resume == nil {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(p.tmp, flags, 0o644)
	if err != nil {
		if os.IsPermission(err) {
			return domain.NewError(domain.ErrPermissionDenied, err, "Sem permissão para escrever em "+p.destDir,
				"Escolha outra pasta de destino.", domain.CertaintyObserved, false)
		}
		return diag.Explain(err, src.Redacted())
	}
	if p.resume == nil {
		// pré-alocação: falha cedo se não houver espaço, e permite WriteAt em qualquer ordem
		if terr := f.Truncate(p.total); terr != nil {
			f.Close()
			if isNoSpace(terr) {
				return domain.NewError(domain.ErrNoSpace, terr, "Espaço em disco insuficiente para este download.",
					"Liberte espaço e tente de novo.", domain.CertaintyObserved, true)
			}
			return diag.Explain(terr, src.Redacted())
		}
		j.seg.Store(newSegState(p.total, p.segSize))
		atomic.StoreInt64(&j.bytesDone, 0)
	}
	seg := j.seg.Load()

	wctx, wcancel := context.WithCancel(j.ctx)
	defer wcancel()
	r := &segRun{e: e, j: j, p: p, f: f, seg: seg, lim: newSharedLimiter(j.task.Policy.SpeedLimitBps),
		ctx: wctx, rawURL: src.URL}

	var (
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) { errOnce.Do(func() { firstErr = err; wcancel() }) }

	// fila de segmentos por concluir
	var qmu sync.Mutex
	var pending []int
	for i := 0; i < seg.count(); i++ {
		if seg.doneOf(i) < seg.length(i) {
			pending = append(pending, i)
		}
	}
	take := func() (int, bool) {
		qmu.Lock()
		defer qmu.Unlock()
		if len(pending) == 0 {
			return 0, false
		}
		i := pending[0]
		pending = pending[1:]
		return i, true
	}

	allowed := int32(p.conns)
	// onRateLimit ajusta a concorrência quando o servidor recusa ligações (429/503):
	// se já há N ligações a funcionar, o servidor aceita pelo menos N (ajusta-se a N);
	// se nenhuma funciona, reduz a metade (no máx. 1x/150 ms). Devolve se ESTE
	// trabalhador deve reformar-se e se já estamos no mínimo de 1 ligação.
	var lastHalve int64
	onRateLimit := func(w int) (retire, atMin bool) {
		a := atomic.LoadInt32(&r.active)
		for {
			cur := atomic.LoadInt32(&allowed)
			nv := cur
			if a >= 1 {
				if a < cur {
					nv = a
				}
			} else {
				now := time.Now().UnixNano()
				prev := atomic.LoadInt64(&lastHalve)
				if now-prev >= int64(150*time.Millisecond) && atomic.CompareAndSwapInt64(&lastHalve, prev, now) {
					if nv = cur / 2; nv < 1 {
						nv = 1
					}
				}
			}
			if nv == cur || atomic.CompareAndSwapInt32(&allowed, cur, nv) {
				now := atomic.LoadInt32(&allowed)
				return int32(w) >= now, now <= 1
			}
		}
	}
	give := func(idx int) {
		qmu.Lock()
		pending = append([]int{idx}, pending...)
		qmu.Unlock()
	}

	// Rondas: um trabalhador reformado devolve o seu segmento à fila, mas os outros
	// podem já ter terminado (a fila estava vazia). Enquanto sobrar trabalho
	// devolvido, lança-se nova ronda (o trabalhador 0 nunca se reforma, por isso
	// há sempre progresso).
	runRound := func() {
		var wg sync.WaitGroup
		for w := 0; w < p.conns; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for wctx.Err() == nil {
					if int32(w) >= atomic.LoadInt32(&allowed) {
						return // concorrência reduzida: este trabalhador reforma-se
					}
					idx, ok := take()
					if !ok {
						return
					}
					if err := r.fetchSegment(idx, w, onRateLimit); err != nil {
						if errors.Is(err, errRetire) {
							give(idx) // outro trabalhador (ou a próxima ronda) acaba este segmento
							return
						}
						fail(err)
						return
					}
				}
			}(w)
		}
		wg.Wait()
	}

	// progresso + persistência periódica
	stopProg := make(chan struct{})
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		interval := e.opts.ProgressEvery
		if interval <= 0 {
			interval = 500 * time.Millisecond
		}
		tick, pers := time.NewTicker(interval), time.NewTicker(persistInterval)
		defer tick.Stop()
		defer pers.Stop()
		last, lastT, ema := atomic.LoadInt64(&j.bytesDone), time.Now(), 0.0
		for {
			select {
			case <-stopProg:
				return
			case <-tick.C:
				cur := atomic.LoadInt64(&j.bytesDone)
				if dt := time.Since(lastT).Seconds(); dt > 0 {
					inst := float64(cur-last) / dt
					if ema == 0 {
						ema = inst
					} else {
						ema = 0.6*ema + 0.4*inst
					}
					atomic.StoreUint64(&j.speedBps, uint64(ema))
				}
				last, lastT = cur, time.Now()
				j.emit(engine.EventProgress, nil)
			case <-pers.C:
				// ordem importa: o snapshot só conta bytes JÁ escritos; o fsync torna-os
				// duráveis; só então se persiste (nunca prometer bytes que podem perder-se).
				sz, done := seg.snapshot()
				if f.Sync() == nil {
					j.persistSnapshot(sz, done)
				}
			}
		}
	}()

	for wctx.Err() == nil {
		runRound()
		qmu.Lock()
		left := len(pending)
		qmu.Unlock()
		if left == 0 {
			break
		}
	}
	close(stopProg)
	<-progDone
	syncErr := f.Sync()

	switch {
	case j.isPaused() && !j.isCancelled():
		j.persistResume()
		f.Close()
		return nil
	case j.isCancelled():
		f.Close()
		return nil
	case firstErr != nil:
		f.Close()
		if errors.Is(firstErr, errFallbackSingle) {
			_ = os.Remove(p.tmp)
			j.clearResume()
			return errFallbackSingle
		}
		if de, ok := firstErr.(*domain.DownloadError); ok && de.Class == domain.ErrResumeMismatch {
			_ = os.Remove(p.tmp) // o recurso mudou: o progresso antigo é inútil
			j.clearResume()
			return de
		}
		if syncErr == nil && seg.sum() > 0 {
			j.persistResume() // progresso preservado para retomar
		}
		if de, ok := firstErr.(*domain.DownloadError); ok {
			return de
		}
		return diag.Explain(firstErr, src.Redacted())
	}
	if syncErr != nil {
		f.Close()
		return diag.Explain(syncErr, src.Redacted())
	}
	if got := seg.sum(); got != p.total {
		f.Close()
		return domain.NewError(domain.ErrCorruptData, nil,
			fmt.Sprintf("Transferência terminou com %d/%d bytes.", got, p.total),
			"Retome a transferência.", domain.CertaintyObserved, true)
	}
	atomic.StoreInt64(&j.bytesDone, p.total)
	if err := f.Close(); err != nil {
		return diag.Explain(err, src.Redacted())
	}
	j.seg.Store(nil)
	return e.finish(j, p.tmp, p.destDir, p.cleanName, p.total, p.src)
}

// persistSnapshot grava a retoma a partir de um snapshot já sincronizado.
func (j *job) persistSnapshot(size int64, done []int64) {
	if j.opts_persist == nil {
		return
	}
	var sum int64
	for _, d := range done {
		sum += d
	}
	if sum <= 0 {
		return
	}
	j.opts_persist(ResumeState{TaskID: j.id, TempPath: j.tempPath, BytesDone: sum,
		Total: atomic.LoadInt64(&j.total), ETag: j.etag, LastModified: j.lastMod,
		CapturedAt: j.capturedAt, SegSize: size, SegDone: done})
}

// fetchSegment transfere um segmento completo, com repetições e backoff.
func (r *segRun) fetchSegment(idx, w int, onRate func(w int) (retire, atMin bool)) error {
	start, end := r.seg.bounds(idx)
	attempts := 0
	for {
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		pos := start + r.seg.doneOf(idx)
		if pos > end {
			return nil
		}
		err := r.readSegment(idx, pos, end, nil)
		if err == nil {
			return nil
		}
		var rl *rateLimitErr
		switch {
		case r.ctx.Err() != nil:
			return r.ctx.Err()
		case errors.Is(err, errFallbackSingle):
			return err
		case errors.As(err, &rl):
			retire, atMin := onRate(w)
			if retire {
				return errRetire
			}
			if atMin { // nem uma única ligação é aceite: conta como falha
				attempts++
				if attempts > r.e.retries() {
					return domain.NewError(domain.ErrHTTPStatus, err, "O servidor recusa o pedido (429/503).",
						"Tente mais tarde; se persistir, a origem limita o acesso.", domain.CertaintyObserved, true)
				}
			}
			wait := rl.retryAfter
			if wait <= 0 {
				wait = r.e.backoff(attempts + 1)
			}
			if !sleepCtx(r.ctx, wait) {
				return r.ctx.Err()
			}
			continue
		}
		if de, ok := err.(*domain.DownloadError); ok && (!de.Retryable || de.Class == domain.ErrNoSpace) {
			return de
		}
		attempts++
		if attempts > r.e.retries() {
			return err
		}
		if !sleepCtx(r.ctx, r.e.backoff(attempts)) {
			return r.ctx.Err()
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// readSegment lê [pos,end] do segmento idx, uma ligação.
func (r *segRun) readSegment(idx int, pos, end int64, body io.ReadCloser) error {
	e, j, p := r.e, r.j, r.p
	if body == nil {
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", e.opts.UserAgent)
		ApplyBrowserHints(req, p.src.Metadata)
		req.Header.Set("Accept-Encoding", "identity") // intervalos de bytes só fazem sentido sem compressão
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, end))
		if p.etag != "" && !strings.HasPrefix(p.etag, "W/") {
			req.Header.Set("If-Range", p.etag)
		} else if p.lastMod != "" {
			req.Header.Set("If-Range", p.lastMod)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			return err
		}
		body = resp.Body
		switch resp.StatusCode {
		case http.StatusPartialContent:
			if verr := validateContentRange(resp.Header.Get("Content-Range"), pos, end); verr != nil {
				body.Close()
				return domain.NewError(domain.ErrResumeMismatch, verr, "Resposta parcial inconsistente com o pedido.",
					"Reinício seguro disponível.", domain.CertaintyObserved, true)
			}
			cr := resp.Header.Get("Content-Range")
			if i := strings.LastIndexByte(cr, '/'); i >= 0 {
				if t, perr := strconv.ParseInt(cr[i+1:], 10, 64); perr == nil && t != p.total {
					body.Close()
					return domain.NewError(domain.ErrResumeMismatch, nil, "O tamanho do recurso mudou durante o download.",
						"Reinício seguro disponível.", domain.CertaintyObserved, true)
				}
			}
		case http.StatusOK:
			// 200 a um pedido Range: ou o servidor ignora Range, ou o validador mudou.
			et := resp.Header.Get("ETag")
			body.Close()
			if p.etag != "" && et != "" && et != p.etag {
				return domain.NewError(domain.ErrResumeMismatch, nil, "O ficheiro mudou no servidor durante o download.",
					"Reinício seguro disponível.", domain.CertaintyObserved, true)
			}
			return errFallbackSingle
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			ra := parseRetryAfter(resp.Header.Get("Retry-After"))
			body.Close()
			return &rateLimitErr{retryAfter: ra}
		case http.StatusRequestedRangeNotSatisfiable:
			body.Close()
			return domain.NewError(domain.ErrResumeMismatch, nil, "O servidor recusou o intervalo pedido (o recurso mudou).",
				"Reinício seguro disponível.", domain.CertaintyObserved, true)
		default:
			code := resp.StatusCode
			body.Close()
			if isExpiredURL(code) {
				return domain.NewError(domain.ErrURLExpired, nil,
					fmt.Sprintf("Servidor recusou o acesso (HTTP %d) — URL provavelmente expirado.", code),
					"Obtenha um novo link na origem e reintroduza-o.", domain.CertaintyObserved, false)
			}
			return domain.NewError(domain.ErrHTTPStatus, nil, fmt.Sprintf("HTTP %d no pedido à origem.", code),
				"Confirme o link; se persistir, a origem está indisponível.", domain.CertaintyObserved, code >= 500)
		}
	}
	defer body.Close()
	atomic.AddInt32(&r.active, 1)
	defer atomic.AddInt32(&r.active, -1)

	// vigilância de bloqueio: sem bytes durante o prazo, fecha-se a ligação
	stall := e.stallTimeout()
	var stalled int32
	tm := time.AfterFunc(stall, func() { atomic.StoreInt32(&stalled, 1); body.Close() })
	defer tm.Stop()

	reader := io.LimitReader(body, end-pos+1)
	buf := make([]byte, 64*1024)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			tm.Reset(stall)
			if werr := writeAt(r.f, buf[:n], pos); werr != nil {
				if isNoSpace(werr) {
					return domain.NewError(domain.ErrNoSpace, werr, "Espaço em disco esgotado.",
						"Liberte espaço e retome; o progresso até aqui foi preservado.", domain.CertaintyObserved, true)
				}
				return diag.Explain(werr, p.src.Redacted())
			}
			pos += int64(n)
			r.seg.add(idx, int64(n))
			done := atomic.AddInt64(&j.bytesDone, int64(n))
			if e.opts.WriteHook != nil {
				e.opts.WriteHook(j.id, done)
			}
			r.lim.wait(r.ctx, n)
		}
		if rerr == io.EOF {
			if pos <= end {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if rerr != nil {
			if atomic.LoadInt32(&stalled) == 1 {
				return errStalled
			}
			return rerr
		}
	}
}

func writeAt(f *os.File, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := f.WriteAt(p, off)
		if err != nil {
			return err
		}
		p, off = p[n:], off+int64(n)
	}
	return nil
}

func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		if n > 30 {
			n = 30
		}
		return time.Duration(n) * time.Second
	}
	return 0
}
