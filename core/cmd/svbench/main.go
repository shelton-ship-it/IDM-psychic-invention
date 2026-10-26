// Comando svbench: mede o ganho da aceleração por várias ligações contra um URL
// REAL (a sua rede, o seu servidor). Descarrega o mesmo ficheiro com 1, 2, 4, 8…
// ligações, apaga-o no fim de cada medição e imprime a velocidade e a aceleração.
//
//	svbench -url https://exemplo.com/ficheiro.iso -connections 1,2,4,8,16
//
// Notas: o ganho depende do servidor (muitos limitam a velocidade POR ligação; se
// limitarem por IP, o ganho é pequeno). A 1.ª medição pode ser penalizada por cache
// fria (DNS, CDN): repita ou use -warmup.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/httpengine"
)

func main() {
	ctx := context.Background()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

type result struct {
	conns   int
	elapsed time.Duration
	bytes   int64
}

func (r result) mbps() float64 { return float64(r.bytes) / (1 << 20) / r.elapsed.Seconds() }

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("svbench", flag.ContinueOnError)
	url := fs.String("url", "", "URL do ficheiro a descarregar (obrigatório)")
	conns := fs.String("connections", "1,2,4,8", "lista de nº de ligações a comparar")
	maxMB := fs.Int64("max-mb", 200, "recusa ficheiros maiores que isto (MiB)")
	timeout := fs.Duration("timeout", 5*time.Minute, "limite por medição")
	warmup := fs.Bool("warmup", false, "faz uma descarga extra descartada antes (aquece DNS/cache)")
	referer := fs.String("referer", "", "Referer a enviar")
	ua := fs.String("user-agent", "", "User-Agent a enviar")
	minSeg := fs.Int64("min-segment-kb", 1024, "tamanho mínimo de segmento (KiB)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("falta -url")
	}
	var list []int
	for _, p := range strings.Split(*conns, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 32 {
			return fmt.Errorf("-connections inválido: %q (valores entre 1 e 32)", p)
		}
		list = append(list, n)
	}
	sort.Ints(list)

	measure := func(n int) (result, error) {
		dir, err := os.MkdirTemp("", "svbench-*")
		if err != nil {
			return result{}, err
		}
		defer os.RemoveAll(dir)
		e := httpengine.New(httpengine.Options{Connections: n, MinSegmentBytes: *minSeg << 10})
		defer e.Close()
		task := domain.DownloadTask{ID: fmt.Sprintf("bench-%d", n), State: domain.StateRunning,
			Source: domain.DownloadSource{Kind: domain.SourceHTTP, URL: *url, Metadata: map[string]string{}},
			Policy: domain.DownloadPolicy{DestinationDir: dir, MaxSizeBytes: *maxMB << 20, AllowResume: true}}
		if *referer != "" {
			task.Source.Metadata["referer"] = *referer
		}
		if *ua != "" {
			task.Source.Metadata["user_agent"] = *ua
		}
		sink := newSink()
		start := time.Now()
		h, err := e.Start(ctx, engine.StartRequest{Task: task, Sink: sink})
		if err != nil {
			return result{}, err
		}
		select {
		case <-sink.fin: // Completed ou Error
		case <-time.After(*timeout):
			h.Cancel(ctx)
			return result{}, fmt.Errorf("%d ligações: excedeu %v", n, *timeout)
		}
		if de := sink.err(); de != nil {
			return result{}, fmt.Errorf("%d ligações: %s %s", n, de.Message, de.Action)
		}
		if !sink.completed() {
			return result{}, fmt.Errorf("%d ligações: não concluiu", n)
		}
		return result{conns: n, elapsed: time.Since(start), bytes: h.Snapshot().BytesDone}, nil
	}

	if *warmup {
		fmt.Fprintln(out, "Aquecimento (descartado)…")
		if _, err := measure(list[0]); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "A medir %s\n\n", redact(*url))
	fmt.Fprintf(out, "%-10s %-10s %-12s %s\n", "ligações", "segundos", "MiB/s", "aceleração")
	var base *result
	for _, n := range list {
		r, err := measure(n)
		if err != nil {
			return err
		}
		if base == nil {
			b := r
			base = &b
		}
		fmt.Fprintf(out, "%-10d %-10.2f %-12.2f ×%.1f\n", r.conns, r.elapsed.Seconds(), r.mbps(), base.elapsed.Seconds()/r.elapsed.Seconds())
	}
	fmt.Fprintln(out, "\nA 1.ª linha é a referência. Se a aceleração for ~×1, o servidor limita por IP, não por ligação.")
	return nil
}

func redact(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i] + "?…"
	}
	return u
}

// sink recolhe eventos (o motor emite de várias goroutines) e sinaliza o fim.
type sink struct {
	mu   sync.Mutex
	evs  []engine.Event
	fin  chan struct{}
	once sync.Once
}

func newSink() *sink { return &sink{fin: make(chan struct{})} }

func (s *sink) Emit(e engine.Event) {
	s.mu.Lock()
	s.evs = append(s.evs, e)
	s.mu.Unlock()
	if e.Kind == engine.EventCompleted || e.Kind == engine.EventError {
		s.once.Do(func() { close(s.fin) })
	}
}

func (s *sink) err() *domain.DownloadError {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.evs {
		if e.Kind == engine.EventError {
			return e.Error
		}
	}
	return nil
}

func (s *sink) completed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.evs {
		if e.Kind == engine.EventCompleted {
			return true
		}
	}
	return false
}
