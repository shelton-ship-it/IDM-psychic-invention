package btnative

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"streamvault/core/app"
	"streamvault/core/config"
	"streamvault/core/domain"
)

// Produto completo: Core.Add(magnet) → motor nativo → verificação por peças →
// Verifying → Completed, tudo contra um seeder local.
func TestCoreAddMagnetCompletesWithNativeEngine(t *testing.T) {
	f := newFx(t)
	seeder := startSeeder(t, f)
	tr := newTracker(t, f.ih, seeder.Addr().String())
	leech := testBackend(t, nil)

	cfg := config.Default()
	cfg.Storage.DefaultDir = t.TempDir()
	st := app.NewMemStore()
	c, err := app.New(cfg, st, app.WithTorrentBackend(leech))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.Add(magnetFor(f, tr.srv.URL).Magnet, domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tarefa concluída", 30*time.Second, func() bool {
		cur, err := st.Get(tk.ID)
		return err == nil && (cur.State == domain.StateCompleted || cur.State == domain.StateFailed)
	})
	cur, _ := st.Get(tk.ID)
	if cur.State != domain.StateCompleted {
		t.Fatalf("estado %s: %+v", cur.State, cur.Error)
	}
	if cur.Policy.RequireIntegrity != domain.IntegrityTorrentPieces {
		t.Errorf("torrents devem exigir verificação por peças: %v", cur.Policy.RequireIntegrity)
	}
	for _, file := range f.meta.Files {
		want, _ := os.ReadFile(filepath.Join(f.src, filepath.FromSlash(file.Path)))
		got, err := os.ReadFile(filepath.Join(cfg.Storage.DefaultDir, "pack", filepath.FromSlash(file.Path)))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s: conteúdo diverge (%v)", file.Path, err)
		}
	}
}
