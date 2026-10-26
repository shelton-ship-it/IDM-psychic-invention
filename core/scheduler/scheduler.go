// Package scheduler agenda o INÍCIO de downloads para uma hora futura (como o
// agendador do IDM). Não faz transferências: à hora marcada chama a função de
// arranque que lhe foi dada (o Core.AddWith), por isso herda toda a política de
// segurança, a fila e a retoma.
//
// Persistência: um ficheiro JSON (escrita atómica, 0600). Credenciais (cookies)
// nunca vão para o disco: uma entrada agendada que precisava de cookie e que
// sobrevive a um reinício falha, com explicação, em vez de correr sem login.
package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	MaxEntries = 1000
	MaxHorizon = 366 * 24 * time.Hour // não se agenda para além de ~1 ano
	maxURLLen  = 4096
)

// Status de uma entrada. Uma entrada que arranca com sucesso é REMOVIDA (passa a
// ser uma tarefa normal da fila); só as que falharam ficam, para o utilizador ver.
type Status string

const (
	StatusPending Status = "pending"
	StatusFailed  Status = "failed"
)

// Entry é um download agendado.
type Entry struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Referer     string    `json:"referer,omitempty"`
	UserAgent   string    `json:"user_agent,omitempty"`
	At          time.Time `json:"at"`
	CreatedAt   time.Time `json:"created_at"`
	Status      Status    `json:"status"`
	Error       string    `json:"error,omitempty"`
	NeedsCookie bool      `json:"needs_cookie,omitempty"`
	Cookie      string    `json:"-"` // só em memória; nunca serializado
}

// StartFunc arranca o download de uma entrada vencida. Um erro marca-a como falhada.
type StartFunc func(Entry) error

// Options do agendador.
type Options struct {
	Now      func() time.Time // relógio (testes); defeito time.Now
	Interval time.Duration    // período da verificação; defeito 1 s
}

type file struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Scheduler guarda e dispara os agendamentos.
type Scheduler struct {
	path  string
	start StartFunc
	now   func() time.Time

	mu      sync.Mutex
	entries []Entry
	running map[string]bool

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// New carrega o ficheiro (se existir) e inicia o ciclo de verificação.
// Um ficheiro ilegível é um erro: nunca se descartam agendamentos em silêncio.
func New(path string, start StartFunc, o Options) (*Scheduler, error) {
	if path == "" || start == nil {
		return nil, errors.New("scheduler: caminho e função de arranque são obrigatórios")
	}
	s := &Scheduler{path: path, start: start, now: o.Now, running: map[string]bool{},
		stop: make(chan struct{}), done: make(chan struct{})}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	iv := o.Interval
	if iv <= 0 {
		iv = time.Second
	}
	go s.loop(iv)
	return s, nil
}

func (s *Scheduler) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("scheduler: ficheiro de agenda ilegível (%s): %w", s.path, err)
	}
	if f.Version != 1 {
		return fmt.Errorf("scheduler: versão %d da agenda não suportada", f.Version)
	}
	for _, e := range f.Entries {
		if e.ID == "" || e.URL == "" {
			continue
		}
		e.Cookie = "" // por desenho nunca vem do disco
		if e.Status != StatusFailed {
			e.Status = StatusPending
		}
		s.entries = append(s.entries, e)
	}
	return nil
}

func (s *Scheduler) save() error {
	f := file{Version: 1, Entries: s.entries}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".schedule-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, s.path)
}

// Add agenda um download. A validação do URL é do chamador (Core.Schedule);
// aqui só se valida o que é da agenda (hora, tamanhos, limites).
func (s *Scheduler) Add(e Entry) (Entry, error) {
	if e.URL == "" || len(e.URL) > maxURLLen {
		return Entry{}, errors.New("url inválida")
	}
	now := s.now()
	if e.At.IsZero() {
		return Entry{}, errors.New("indique a hora (campo \"at\", RFC 3339)")
	}
	if e.At.After(now.Add(MaxHorizon)) {
		return Entry{}, errors.New("não se pode agendar com mais de um ano de antecedência")
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Entry{}, err
	}
	e.ID = hex.EncodeToString(id[:])
	e.CreatedAt = now.UTC()
	e.At = e.At.UTC()
	e.Status, e.Error = StatusPending, ""
	e.NeedsCookie = e.Cookie != ""

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= MaxEntries {
		return Entry{}, fmt.Errorf("limite de %d agendamentos atingido; remova alguns", MaxEntries)
	}
	s.entries = append(s.entries, e)
	if err := s.save(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return Entry{}, fmt.Errorf("não foi possível guardar a agenda: %w", err)
	}
	return scrub(e), nil
}

// List devolve as entradas por hora (sem credenciais).
func (s *Scheduler) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, scrub(e))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Remove apaga uma entrada (pendente ou falhada). Não cancela uma que está a arrancar.
func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return errors.New("o agendamento está a arrancar neste momento")
	}
	for i, e := range s.entries {
		if e.ID == id {
			prev := append([]Entry(nil), s.entries...)
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			if err := s.save(); err != nil {
				s.entries = prev
				return fmt.Errorf("não foi possível guardar a agenda: %w", err)
			}
			return nil
		}
	}
	return errors.New("agendamento inexistente")
}

func scrub(e Entry) Entry { e.Cookie = ""; return e }

// RunDue dispara as entradas pendentes cuja hora já passou. O ciclo interno
// chama-a periodicamente; é exportada para testes.
func (s *Scheduler) RunDue() {
	now := s.now()
	s.mu.Lock()
	var due []Entry
	for _, e := range s.entries {
		if e.Status == StatusPending && !e.At.After(now) && !s.running[e.ID] {
			due = append(due, e)
			s.running[e.ID] = true
		}
	}
	s.mu.Unlock()

	for _, e := range due {
		err := s.runOne(e)
		s.mu.Lock()
		delete(s.running, e.ID)
		for i := range s.entries {
			if s.entries[i].ID != e.ID {
				continue
			}
			if err == nil {
				s.entries = append(s.entries[:i], s.entries[i+1:]...)
			} else {
				s.entries[i].Status, s.entries[i].Error = StatusFailed, err.Error()
				s.entries[i].Cookie = ""
			}
			break
		}
		_ = s.save() // melhor esforço: o estado em memória já está certo
		s.mu.Unlock()
	}
}

func (s *Scheduler) runOne(e Entry) (err error) {
	if e.NeedsCookie && e.Cookie == "" {
		return errors.New("este agendamento precisava de credenciais (cookies), que só existem em memória e perderam-se ao reiniciar; agende de novo a partir do navegador com a sessão iniciada")
	}
	defer func() {
		if r := recover(); r != nil { // um erro no arranque não pode matar o ciclo
			err = fmt.Errorf("falha interna ao iniciar: %v", r)
		}
	}()
	return s.start(e)
}

func (s *Scheduler) loop(iv time.Duration) {
	defer close(s.done)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.RunDue()
		}
	}
}

// Close pára o ciclo e apaga as credenciais da memória. As entradas pendentes
// ficam no ficheiro para a próxima execução.
func (s *Scheduler) Close() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		for i := range s.entries {
			s.entries[i].Cookie = ""
		}
		s.mu.Unlock()
	})
}
