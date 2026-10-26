package scheduler

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type starts struct {
	mu   sync.Mutex
	got  []Entry
	fail error
}

func (s *starts) fn(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
	return s.fail
}
func (s *starts) n() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) }

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// newSched usa um Interval enorme: o ciclo interno nunca dispara, os testes
// chamam RunDue à mão com o relógio falso.
func newSched(t *testing.T, path string, st *starts, c *clock) *Scheduler {
	t.Helper()
	s, err := New(path, st.fn, Options{Now: c.now, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestStartsOnlyWhenDueAndThenRemovesTheEntry(t *testing.T) {
	c, st := &clock{t: t0}, &starts{}
	s := newSched(t, filepath.Join(t.TempDir(), "agenda.json"), st, c)
	e, err := s.Add(Entry{URL: "https://x.example.com/f.zip", Referer: "https://x.example.com/", UserAgent: "UA", At: t0.Add(time.Hour)})
	if err != nil || e.ID == "" || e.Status != StatusPending {
		t.Fatalf("Add: %+v %v", e, err)
	}
	s.RunDue()
	if st.n() != 0 {
		t.Fatal("não pode arrancar antes da hora")
	}
	c.add(59 * time.Minute)
	s.RunDue()
	if st.n() != 0 {
		t.Fatal("falta 1 minuto")
	}
	c.add(time.Minute) // exatamente à hora
	s.RunDue()
	if st.n() != 1 || st.got[0].URL != "https://x.example.com/f.zip" || st.got[0].Referer == "" || st.got[0].UserAgent != "UA" {
		t.Fatalf("devia arrancar uma vez com os dados da entrada: %+v", st.got)
	}
	if l := s.List(); len(l) != 0 {
		t.Errorf("arrancou: deixa de estar na agenda (passou a ser uma tarefa): %+v", l)
	}
	s.RunDue()
	if st.n() != 1 {
		t.Error("não pode arrancar duas vezes")
	}
}

func TestPastDueEntryRunsOnNextTick(t *testing.T) {
	c, st := &clock{t: t0}, &starts{}
	s := newSched(t, filepath.Join(t.TempDir(), "a.json"), st, c)
	if _, err := s.Add(Entry{URL: "https://x.example.com/f.zip", At: t0.Add(-3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	s.RunDue()
	if st.n() != 1 {
		t.Fatal("uma hora já passada arranca de imediato")
	}
}

func TestFailureKeepsTheEntryWithTheReasonAndDoesNotRetryForever(t *testing.T) {
	c, st := &clock{t: t0}, &starts{fail: errors.New("bloqueado pela política")}
	s := newSched(t, filepath.Join(t.TempDir(), "a.json"), st, c)
	e, _ := s.Add(Entry{URL: "https://x.example.com/f.zip", At: t0})
	s.RunDue()
	s.RunDue()
	if st.n() != 1 {
		t.Fatalf("uma entrada falhada não é repetida em ciclo: %d arranques", st.n())
	}
	l := s.List()
	if len(l) != 1 || l[0].ID != e.ID || l[0].Status != StatusFailed || !strings.Contains(l[0].Error, "bloqueado") {
		t.Fatalf("a falha fica visível com o motivo: %+v", l)
	}
	if err := s.Remove(e.ID); err != nil || len(s.List()) != 0 {
		t.Fatalf("o utilizador remove a entrada falhada: %v", err)
	}
}

func TestPanicInStartIsContainedAndMarksFailure(t *testing.T) {
	c := &clock{t: t0}
	s, err := New(filepath.Join(t.TempDir(), "a.json"), func(Entry) error { panic("boom") }, Options{Now: c.now, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Add(Entry{URL: "https://x.example.com/f.zip", At: t0})
	s.RunDue() // não pode propagar o pânico
	if l := s.List(); len(l) != 1 || l[0].Status != StatusFailed {
		t.Fatalf("esperava failed: %+v", l)
	}
}

func TestPersistsAcrossRestartAndFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "agenda.json")
	c, st := &clock{t: t0}, &starts{}
	s1, err := New(path, st.fn, Options{Now: c.now, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s1.Add(Entry{URL: "https://x.example.com/f.zip", At: t0.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // o Windows não tem permissões POSIX
		t.Fatalf("ficheiro da agenda: perm=%v (esperado 0600)", fi.Mode().Perm())
	}
	s2, err := New(path, st.fn, Options{Now: c.now, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l := s2.List()
	if len(l) != 1 || l[0].ID != e.ID || !l[0].At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("a agenda devia sobreviver ao reinício: %+v", l)
	}
	c.add(2 * time.Hour)
	s2.RunDue()
	if st.n() != 1 {
		t.Fatal("a entrada recarregada devia arrancar à hora")
	}
}

// Credenciais: nunca no disco; após reinício a entrada falha com explicação
// (nunca corre sem login).
func TestCookieIsNeverWrittenAndIsLostAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenda.json")
	c, st := &clock{t: t0}, &starts{}
	s1, _ := New(path, st.fn, Options{Now: c.now, Interval: time.Hour})
	e, err := s1.Add(Entry{URL: "https://x.example.com/privado.zip", At: t0.Add(time.Hour), Cookie: "sid=SEGREDO-AGENDA"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Cookie != "" || !e.NeedsCookie {
		t.Fatalf("o valor devolvido não repete o cookie: %+v", e)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "SEGREDO-AGENDA") {
		t.Fatalf("cookie gravado em disco: %s", raw)
	}
	for _, l := range s1.List() {
		if l.Cookie != "" {
			t.Fatal("List não pode devolver credenciais")
		}
	}

	// mesmo processo: o cookie está em memória e chega ao arranque
	c.add(time.Hour)
	s1.RunDue()
	if st.n() != 1 || st.got[0].Cookie != "sid=SEGREDO-AGENDA" {
		t.Fatalf("no mesmo processo o cookie chega ao arranque: %+v", st.got)
	}
	s1.Close()

	// reinício: agenda com NeedsCookie mas sem o valor
	s1b, _ := New(path, st.fn, Options{Now: c.now, Interval: time.Hour})
	s1b.Add(Entry{URL: "https://x.example.com/outro.zip", At: t0.Add(2 * time.Hour), Cookie: "sid=OUTRO"})
	s1b.Close()
	st2 := &starts{}
	s2, _ := New(path, st2.fn, Options{Now: c.now, Interval: time.Hour})
	defer s2.Close()
	c.add(2 * time.Hour)
	s2.RunDue()
	if st2.n() != 0 {
		t.Fatal("sem credenciais não pode arrancar o download")
	}
	l := s2.List()
	if len(l) != 1 || l[0].Status != StatusFailed || !strings.Contains(l[0].Error, "credenciais") {
		t.Fatalf("esperava falha explicada sobre as credenciais: %+v", l)
	}
}

func TestValidationLimitsAndCorruptFile(t *testing.T) {
	dir := t.TempDir()
	c, st := &clock{t: t0}, &starts{}
	s := newSched(t, filepath.Join(dir, "a.json"), st, c)
	for name, e := range map[string]Entry{
		"sem url":         {At: t0},
		"sem hora":        {URL: "https://x.example.com/f.zip"},
		"url enorme":      {URL: "https://x.example.com/" + strings.Repeat("a", 5000), At: t0},
		"demasiado longe": {URL: "https://x.example.com/f.zip", At: t0.Add(400 * 24 * time.Hour)},
	} {
		if _, err := s.Add(e); err == nil {
			t.Errorf("%s: devia ser recusado", name)
		}
	}
	if len(s.List()) != 0 {
		t.Error("nada devia ter ficado agendado")
	}

	bad := filepath.Join(dir, "corrupto.json")
	os.WriteFile(bad, []byte("{isto não é json"), 0o600)
	if _, err := New(bad, st.fn, Options{Now: c.now}); err == nil {
		t.Error("uma agenda ilegível é um erro (não se descartam agendamentos em silêncio)")
	}
	fut := filepath.Join(dir, "futuro.json")
	os.WriteFile(fut, []byte(`{"version":99,"entries":[]}`), 0o600)
	if _, err := New(fut, st.fn, Options{Now: c.now}); err == nil {
		t.Error("versão desconhecida devia ser recusada")
	}
}

func TestRemoveAndOrdering(t *testing.T) {
	c, st := &clock{t: t0}, &starts{}
	s := newSched(t, filepath.Join(t.TempDir(), "a.json"), st, c)
	late, _ := s.Add(Entry{URL: "https://x.example.com/2.zip", At: t0.Add(5 * time.Hour)})
	early, _ := s.Add(Entry{URL: "https://x.example.com/1.zip", At: t0.Add(time.Hour)})
	l := s.List()
	if len(l) != 2 || l[0].ID != early.ID || l[1].ID != late.ID {
		t.Fatalf("lista por hora: %+v", l)
	}
	if err := s.Remove(early.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(early.ID); err == nil {
		t.Error("remover duas vezes devia dar erro")
	}
	c.add(2 * time.Hour)
	s.RunDue()
	if st.n() != 0 {
		t.Fatal("uma entrada removida não arranca")
	}
}

func TestBackgroundLoopFiresWithoutManualTicks(t *testing.T) {
	st := &starts{}
	s, err := New(filepath.Join(t.TempDir(), "a.json"), st.fn, Options{Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Add(Entry{URL: "https://x.example.com/f.zip", At: time.Now().Add(30 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for st.n() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("o ciclo interno devia disparar a entrada")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
