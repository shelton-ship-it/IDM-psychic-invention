package hlsengine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"streamvault/core/diag"
	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/integrity"
)

type rateLimitErr struct{ retryAfter time.Duration }

func (r *rateLimitErr) Error() string { return "servidor limitou os pedidos (429/503)" }

var errRetire = errors.New("trabalhador reformado")

type hlsRun struct {
	e     *Engine
	j     *job
	media *Playlist
	work  string
	out   *os.File
	st    *state
	sum   hash.Hash
	guard *urlGuard
	lim   *sharedLimiter

	// várias faixas: bytes já concluídos das anteriores (para o progresso acumulado)
	// e se o total ainda é desconhecido (há faixas por descarregar a seguir)
	base         int64
	unknownTotal bool

	mu        sync.Mutex
	completed map[int]int64 // segmentos descarregados, ainda não anexados
	pending   []int         // índices por descarregar (ascendentes)
	appended  int           // espelho de st.Appended (para a janela)
	segBytes  int64         // bytes de segmentos completos (para estimar o total)
	segCount  int
	active    int32
	wake      chan struct{}
	window    int
}

func (r *hlsRun) conns() int {
	c := r.e.opts.Connections
	if p := r.j.task.Policy.MaxConcurrency; p > 0 {
		c = p
	}
	if c > 16 {
		c = 16
	}
	if c < 1 {
		c = 1
	}
	return c
}

func (r *hlsRun) segPath(i int, ext string) string {
	return filepath.Join(r.work, fmt.Sprintf("seg-%06d.%s", i, ext))
}

// attempt faz UM pedido e grava o corpo em dst (via .part). Devolve o tamanho.
func (r *hlsRun) attempt(ctx context.Context, rawURL, dst string) (int64, error) {
	resp, err := r.e.get(ctx, r.j, rawURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return 0, &rateLimitErr{retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		return 0, statusError(resp.StatusCode, false)
	}
	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	var written int64
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(part)
			atomic.AddInt64(&r.j.bytesDone, -written) // o que se contou deste pedido falhado
		}
	}()
	stall := r.e.opts.StallTimeout
	var stalled int32
	tm := time.AfterFunc(stall, func() { atomic.StoreInt32(&stalled, 1); resp.Body.Close() })
	defer tm.Stop()
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			tm.Reset(stall)
			if _, werr := f.Write(buf[:n]); werr != nil {
				if isNoSpace(werr) {
					return 0, domain.NewError(domain.ErrNoSpace, werr, "Espaço em disco esgotado.",
						"Liberte espaço e retome.", domain.CertaintyObserved, true)
				}
				return 0, werr
			}
			written += int64(n)
			atomic.AddInt64(&r.j.bytesDone, int64(n))
			r.lim.wait(ctx, n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if atomic.LoadInt32(&stalled) == 1 {
				return 0, errors.New("sem dados dentro do tempo limite")
			}
			return 0, rerr
		}
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return 0, io.ErrUnexpectedEOF
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(part, dst); err != nil {
		return 0, err
	}
	ok = true
	return written, nil
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

func (r *hlsRun) backoff(attempt int) time.Duration {
	d := r.e.opts.SegmentBackoff << uint(attempt-1)
	if d > 8*time.Second || d <= 0 {
		d = 8 * time.Second
	}
	return d
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

func isNoSpace(err error) bool {
	return err != nil && (strings.Contains(strings.ToLower(err.Error()), "no space left") ||
		strings.Contains(strings.ToLower(err.Error()), "not enough space") ||
		strings.Contains(strings.ToLower(err.Error()), "disk full"))
}

// fetchRetry: com repetições; onRate==nil => pedido sequencial (init).
func (r *hlsRun) fetchRetry(ctx context.Context, rawURL, dst string, w int, onRate func(int) (bool, bool)) (int64, error) {
	attempts := 0
	for {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		n, err := r.attempt(ctx, rawURL, dst)
		if err == nil {
			return n, nil
		}
		var rl *rateLimitErr
		switch {
		case ctx.Err() != nil:
			return 0, ctx.Err()
		case errors.As(err, &rl):
			if onRate != nil {
				retire, atMin := onRate(w)
				if retire {
					return 0, errRetire
				}
				if !atMin {
					if !sleepCtx(ctx, maxDur(rl.retryAfter, r.backoff(1))) {
						return 0, ctx.Err()
					}
					continue
				}
			}
			attempts++
			if attempts > r.e.opts.SegmentRetries {
				return 0, domain.NewError(domain.ErrHTTPStatus, err, "O servidor recusa os pedidos (429/503).",
					"Tente mais tarde; se persistir, a origem limita o acesso.", domain.CertaintyObserved, true)
			}
			if !sleepCtx(ctx, maxDur(rl.retryAfter, r.backoff(attempts))) {
				return 0, ctx.Err()
			}
			continue
		}
		if de, ok := err.(*domain.DownloadError); ok && (!de.Retryable || de.Class == domain.ErrNoSpace) {
			return 0, de
		}
		attempts++
		if attempts > r.e.opts.SegmentRetries {
			return 0, err
		}
		if !sleepCtx(ctx, r.backoff(attempts)) {
			return 0, ctx.Err()
		}
	}
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (r *hlsRun) take() (idx int, ok, gated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return 0, false, false
	}
	if r.pending[0] >= r.appended+r.window {
		return 0, false, true
	}
	idx = r.pending[0]
	r.pending = r.pending[1:]
	return idx, true, false
}

func (r *hlsRun) give(idx int) {
	r.mu.Lock()
	r.pending = append([]int{idx}, r.pending...)
	r.mu.Unlock()
}

func (r *hlsRun) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// execute descarrega e anexa todos os segmentos (ou até pausa/cancelamento/erro).
func (r *hlsRun) execute() error {
	j, e := r.j, r.e
	N := len(r.media.Segments)
	r.completed = map[int]int64{}
	r.wake = make(chan struct{}, 1)
	r.window = e.opts.Window
	if r.window <= 0 {
		r.window = 4*r.conns() + 16
	}
	if sp := j.task.Policy.SpeedLimitBps; sp > 0 {
		r.lim = &sharedLimiter{bps: float64(sp)}
	}
	r.appended = r.st.Appended
	r.segCount = r.st.Appended
	r.segBytes = r.st.AppendedBytes - r.st.InitBytes
	atomic.StoreInt64(&j.bytesDone, r.base+r.st.AppendedBytes)
	r.updateEstimate()

	wctx, wcancel := context.WithCancel(j.ctx)
	defer wcancel()
	var (
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) { errOnce.Do(func() { firstErr = err; wcancel() }) }

	// segmento de inicialização (fMP4)
	if r.media.MapURL != "" && !r.st.InitDone {
		dst := filepath.Join(r.work, "init.bin")
		n, err := r.fetchRetry(wctx, r.media.MapURL, dst, 0, nil)
		if err != nil {
			return r.settle(err, wctx)
		}
		if err := r.appendFile(dst, n); err != nil {
			return r.settle(err, wctx)
		}
		r.mu.Lock()
		r.st.InitDone, r.st.InitBytes = true, n
		r.mu.Unlock()
	}
	for i := r.st.Appended; i < N; i++ {
		r.pending = append(r.pending, i)
	}

	// progresso + pontos de controlo
	stopProg := make(chan struct{})
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		tick, cp := time.NewTicker(e.opts.ProgressEvery), time.NewTicker(2*time.Second)
		defer tick.Stop()
		defer cp.Stop()
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
			case <-cp.C:
				_ = r.checkpoint()
			}
		}
	}()

	// anexador: escreve os segmentos por ORDEM no ficheiro final
	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		for next := r.st.Appended; next < N; {
			r.mu.Lock()
			size, ready := r.completed[next]
			r.mu.Unlock()
			if !ready {
				select {
				case <-r.wake:
				case <-wctx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
			if err := r.appendFile(r.segPath(next, "bin"), size); err != nil {
				fail(err)
				return
			}
			r.mu.Lock()
			delete(r.completed, next)
			r.st.Appended = next + 1
			r.st.AppendedBytes += size
			r.appended = next + 1
			r.mu.Unlock()
			next++
		}
	}()

	// trabalhadores (em rondas: um trabalhador reformado devolve o segmento à fila)
	allowed := int32(r.conns())
	var lastHalve int64
	onRate := func(w int) (retire, atMin bool) {
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
	runRound := func() {
		var wg sync.WaitGroup
		for w := 0; w < r.conns(); w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for wctx.Err() == nil {
					if int32(w) >= atomic.LoadInt32(&allowed) {
						return
					}
					idx, ok, gated := r.take()
					if gated {
						if !sleepCtx(wctx, 20*time.Millisecond) {
							return
						}
						continue
					}
					if !ok {
						return
					}
					n, err := func() (int64, error) {
						atomic.AddInt32(&r.active, 1)
						defer atomic.AddInt32(&r.active, -1)
						return r.fetchRetry(wctx, r.media.Segments[idx].URL, r.segPath(idx, "bin"), w, onRate)
					}()
					if err != nil {
						if errors.Is(err, errRetire) {
							r.give(idx)
							return
						}
						fail(err)
						return
					}
					r.mu.Lock()
					r.completed[idx] = n
					r.segBytes += n
					r.segCount++
					r.mu.Unlock()
					r.updateEstimate()
					r.notify()
				}
			}(w)
		}
		wg.Wait()
	}
	for wctx.Err() == nil {
		runRound()
		r.mu.Lock()
		left := len(r.pending)
		r.mu.Unlock()
		if left == 0 {
			break
		}
	}
	<-appDone
	close(stopProg)
	<-progDone
	return r.settle(firstErr, wctx)
}

// settle fecha a execução: guarda o ponto de controlo e devolve o erro (ou nil).
func (r *hlsRun) settle(err error, ctx context.Context) error {
	cpErr := r.checkpoint()
	if r.j.isPaused() || r.j.isCancelled() {
		return nil
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		if de, ok := err.(*domain.DownloadError); ok {
			return de
		}
		return diag.Explain(err, r.j.task.Source.Redacted())
	}
	if cpErr != nil {
		return diag.Explain(cpErr, r.j.task.Source.Redacted())
	}
	if ctx.Err() != nil && r.appendedAll() == false {
		return context.Canceled
	}
	return nil
}

func (r *hlsRun) appendedAll() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Appended >= len(r.media.Segments)
}

func (r *hlsRun) updateEstimate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.segCount == 0 || r.unknownTotal {
		return // com faixas por descarregar o total é desconhecido (-1), não um palpite errado
	}
	avg := float64(r.segBytes) / float64(r.segCount)
	est := r.base + int64(avg*float64(len(r.media.Segments))) + r.st.InitBytes
	if done := atomic.LoadInt64(&r.j.bytesDone); est < done {
		est = done
	}
	atomic.StoreInt64(&r.j.bytesTotal, est)
}

// appendFile anexa um ficheiro ao resultado (e ao hash), apagando-o depois.
func (r *hlsRun) appendFile(path string, want int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(r.out, r.sum), f)
	f.Close()
	if err != nil {
		if isNoSpace(err) {
			return domain.NewError(domain.ErrNoSpace, err, "Espaço em disco esgotado.",
				"Liberte espaço e retome.", domain.CertaintyObserved, true)
		}
		return err
	}
	if n != want {
		return fmt.Errorf("segmento com %d bytes (esperado %d)", n, want)
	}
	if strings.HasSuffix(path, "init.bin") {
		r.mu.Lock()
		r.st.AppendedBytes += n
		r.mu.Unlock()
	}
	_ = os.Remove(path)
	return nil
}

// checkpoint: fsync do resultado e SÓ DEPOIS grava o estado (nunca prometer bytes
// que podem perder-se).
func (r *hlsRun) checkpoint() error {
	r.mu.Lock()
	snap := *r.st
	r.mu.Unlock()
	if err := r.out.Sync(); err != nil {
		return err
	}
	return snap.save(r.work)
}

// finish verifica a integridade e conclui (rename atómico, sem sobrescrever).
func (r *hlsRun) finish(outPath, destDir, cleanName string) error {
	if err := r.out.Sync(); err != nil {
		return diag.Explain(err, r.j.task.Source.Redacted())
	}
	size := r.st.AppendedBytes
	shaHex := hex.EncodeToString(r.sum.Sum(nil))
	if err := r.j.verify(outPath); err != nil {
		return err
	}
	if err := r.out.Close(); err != nil {
		return diag.Explain(err, r.j.task.Source.Redacted())
	}
	return r.j.publish(r.work, outPath, destDir, cleanName, size, shaHex)
}

// complete conclui um resultado já escrito em outPath (várias faixas juntas): verifica
// a integridade, move-o para o nome final e emite os eventos.
func (j *job) complete(work, outPath, destDir, cleanName string, size int64, shaHex string) error {
	if err := j.verify(outPath); err != nil {
		return err
	}
	return j.publish(work, outPath, destDir, cleanName, size, shaHex)
}

// verify aplica a política de integridade do pedido (SHA-256 esperado), se houver.
func (j *job) verify(outPath string) error {
	src := j.task.Source
	if j.task.Policy.RequireIntegrity != domain.IntegritySHA256 {
		return nil
	}
	expected := ""
	for _, fm := range j.task.Files {
		if fm.Index == 0 {
			expected = fm.SHA256Hex
		}
	}
	res, actual, verr := integrity.VerifyFile(outPath, expected)
	if verr != nil {
		return diag.Explain(verr, src.Redacted())
	}
	if res != integrity.ResultOK {
		j.emit(engine.EventIntegrityReport, map[string]string{"result": res.String(), "expected": expected, "actual": actual})
		return domain.NewError(domain.ErrIntegrityFailure, nil, "Verificação SHA-256 falhou ("+res.String()+").",
			"O ficheiro foi mantido para diagnóstico; apague-o e tente de novo.", domain.CertaintyObserved, true)
	}
	return nil
}

// publish move o resultado para o nome final (sem sobrescrever), limpa o trabalho e emite os eventos.
func (j *job) publish(work, outPath, destDir, cleanName string, size int64, shaHex string) error {
	src := j.task.Source
	finalPath := filepath.Join(destDir, cleanName)
	if _, err := os.Stat(finalPath); err == nil {
		finalPath = uniquePath(finalPath)
	}
	if err := os.Rename(outPath, finalPath); err != nil {
		return diag.Explain(err, src.Redacted())
	}
	clearDir(work)
	_ = os.RemoveAll(work) // várias faixas: contém uma subpasta por faixa (sempre destino/.svhls-<id>)
	atomic.StoreInt64(&j.bytesDone, size)
	atomic.StoreInt64(&j.bytesTotal, size)
	j.emit(engine.EventFileDiscovered, map[string]string{"path": finalPath, "sha256": shaHex, "size": fmt.Sprint(size)})
	j.emit(engine.EventCompleted, map[string]string{"path": finalPath, "sha256": shaHex})
	return nil
}

func uniquePath(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			return c
		}
	}
	return fmt.Sprintf("%s (%d)%s", base, os.Getpid(), ext)
}
