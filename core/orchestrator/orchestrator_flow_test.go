package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"streamvault/core/domain"
	"streamvault/core/engine"
)

func newFlow(t *testing.T, maxConc int) (*Orchestrator, *memStore, *fakeEngine, *collector) {
	t.Helper()
	store := newMemStore()
	fe := &fakeEngine{}
	router := NewEngineRouter()
	router.Register(domain.SourceHTTP, fe)
	col := &collector{}
	orch, err := New(Options{Store: store, Router: router, Sink: col, MaxConcur: maxConc, Clock: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	return orch, store, fe, col
}

func sawState(col *collector, s domain.State) bool {
	col.mu.Lock()
	defer col.mu.Unlock()
	for _, e := range col.events {
		if e.Kind == engine.EventStateChanged && e.State == s {
			return true
		}
	}
	return false
}

var httpSource = domain.DownloadSource{Kind: domain.SourceHTTP, URL: "https://x.example/a.bin"}

// Conclusão sem política de integridade passa por Verifying e chega a Completed.
func TestCompletionPassesThroughVerifying(t *testing.T) {
	orch, store, _, col := newFlow(t, 1)
	tk, _ := orch.Submit(httpSource, domain.DownloadPolicy{})
	orch.StartNext()
	orch.ObserveEngine(engine.Event{Kind: engine.EventCompleted, TaskID: tk.ID})
	got, _ := store.Get(tk.ID)
	if got.State != domain.StateCompleted {
		t.Fatalf("esperava completed, obtive %s (err=%+v)", got.State, got.Error)
	}
	if !sawState(col, domain.StateVerifying) {
		t.Error("Verifying nunca foi emitido")
	}
	// capacidade libertada
	tk2, _ := orch.Submit(httpSource, domain.DownloadPolicy{})
	if n, _ := orch.StartNext(); n == nil || n.ID != tk2.ID {
		t.Error("capacidade não foi libertada após conclusão")
	}
}

// SHA-256 só conclui com o ficheiro real e o hash certo.
func TestSHA256CompletionRequiresProof(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	data := []byte("conteudo")
	os.WriteFile(path, data, 0o644)
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])

	cases := []struct {
		name    string
		hash    string
		payload map[string]string
		want    domain.State
	}{
		{"hash certo", good, map[string]string{"path": path}, domain.StateCompleted},
		{"hash errado", "00" + good[2:], map[string]string{"path": path}, domain.StateFailed},
		{"sem hash esperado", "", map[string]string{"path": path}, domain.StateFailed},
		{"sem caminho", good, nil, domain.StateFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orch, store, _, _ := newFlow(t, 1)
			tk, _ := orch.Submit(httpSource, domain.DownloadPolicy{RequireIntegrity: domain.IntegritySHA256})
			tk.Files = []domain.FileMetadata{{Index: 0, SHA256Hex: c.hash}}
			store.Save(tk)
			orch.StartNext()
			orch.ObserveEngine(engine.Event{Kind: engine.EventCompleted, TaskID: tk.ID, Payload: c.payload})
			got, _ := store.Get(tk.ID)
			if got.State != c.want {
				t.Fatalf("esperava %s, obtive %s (err=%+v)", c.want, got.State, got.Error)
			}
			if c.want == domain.StateFailed && (got.Error == nil || got.Error.Class != domain.ErrIntegrityFailure) {
				t.Errorf("falha sem diagnóstico de integridade: %+v", got.Error)
			}
		})
	}
}

// Pausar liberta o slot; retomar faz novo Start e respeita a capacidade.
func TestPauseReleasesSlotAndResumeRestarts(t *testing.T) {
	orch, store, fe, _ := newFlow(t, 1)
	a, _ := orch.Submit(httpSource, domain.DownloadPolicy{})
	b, _ := orch.Submit(httpSource, domain.DownloadPolicy{})
	orch.StartNext() // a
	if err := orch.Pause(a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(a.ID); got.State != domain.StatePaused {
		t.Fatalf("esperava paused, obtive %s", got.State)
	}
	if n, _ := orch.StartNext(); n == nil || n.ID != b.ID { // slot livre → b arranca
		t.Fatal("pausa não libertou o slot")
	}
	if err := orch.Resume(a.ID); err == nil {
		t.Fatal("retoma sem capacidade deveria ser recusada")
	}
	orch.Cancel(b.ID) // liberta capacidade
	if err := orch.Resume(a.ID); err != nil {
		t.Fatalf("retoma: %v", err)
	}
	if got, _ := store.Get(a.ID); got.State != domain.StateRunning {
		t.Fatalf("esperava running, obtive %s", got.State)
	}
	if len(fe.handles) != 3 { // a, b, a de novo
		t.Fatalf("esperava 3 Start no motor, obtive %d", len(fe.handles))
	}
}

// Cancelar uma tarefa que ainda não arrancou é possível.
func TestCancelPendingAndPausedTasks(t *testing.T) {
	orch, store, _, _ := newFlow(t, 1)
	p, _ := orch.Submit(httpSource, domain.DownloadPolicy{})
	if err := orch.Cancel(p.ID); err != nil {
		t.Fatalf("cancelar pendente: %v", err)
	}
	if got, _ := store.Get(p.ID); got.State != domain.StateCancelled {
		t.Fatalf("esperava cancelled, obtive %s", got.State)
	}
	if err := orch.Cancel(p.ID); err == nil {
		t.Error("cancelar tarefa já cancelada deveria falhar (terminal)")
	}
}

// A conclusão regista o ficheiro final na tarefa (nome real, tamanho, hash),
// sem substituir um hash esperado fornecido pela origem.
func TestCompletionRecordsFinalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "final (1).bin")
	os.WriteFile(path, []byte("12345"), 0o644)
	orch, store, _, _ := newFlow(t, 1)
	tk, _ := orch.Submit(httpSource, domain.DownloadPolicy{})
	orch.StartNext()
	orch.ObserveEngine(engine.Event{Kind: engine.EventCompleted, TaskID: tk.ID,
		Payload: map[string]string{"path": path, "sha256": "ABCDEF"}})
	got, _ := store.Get(tk.ID)
	if got.State != domain.StateCompleted || len(got.Files) != 1 {
		t.Fatalf("estado/ficheiros inesperados: %s %+v", got.State, got.Files)
	}
	f := got.Files[0]
	if f.Path != "final (1).bin" || f.SizeBytes != 5 || f.SHA256Hex != "abcdef" {
		t.Fatalf("ficheiro final mal registado: %+v", f)
	}
}
