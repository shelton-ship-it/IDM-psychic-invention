package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"streamvault/core/domain"
)

const dashFixtures = "../mp4merge/testdata/dash/"

// dashServer serve o manifesto real do ffmpeg (como /aula.mpd) e os seus segmentos;
// com cookie != "" exige-o em TODOS os pedidos e regista os que chegam sem ele.
func dashServer(t *testing.T, cookie string) (*httptest.Server, *cookieLog) {
	t.Helper()
	files := map[string][]byte{}
	es, err := os.ReadDir(dashFixtures)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		b, err := os.ReadFile(dashFixtures + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		name := "/" + e.Name()
		if e.Name() == "manifest.mpd" {
			name = "/aula.mpd"
		}
		files[name] = b
	}
	lg := &cookieLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lg.add(r)
		if cookie != "" && r.Header.Get("Cookie") != cookie {
			http.Error(w, "login", http.StatusForbidden)
			return
		}
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, lg
}

func tracksOf(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("MP4 ilegível: %v", err)
	}
	out := map[string]bool{}
	for _, tr := range f.Moov.Traks {
		out[tr.Mdia.Hdlr.HandlerType] = true
	}
	return out
}

func TestEndToEndDASHVideoAndAudioBecomeOnePlayableMP4(t *testing.T) {
	srv, _ := dashServer(t, "")
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.Add(srv.URL+"/aula.mpd", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Source.Kind != domain.SourceDASH {
		t.Fatalf("o .mpd devia ser reconhecido como DASH (adaptador antes do HTTP), obtido %q", tk.Source.Kind)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	got := tracksOf(t, filepath.Join(dir, "aula.mp4"))
	if !got["vide"] || !got["soun"] || len(got) != 2 {
		t.Fatalf("esperava vídeo + áudio no mesmo ficheiro: %v", got)
	}
}

// DASH atrás de login: o cookie vai ao manifesto E aos segmentos do mesmo host.
func TestDASHBehindLoginUsesCookieOnEveryRequest(t *testing.T) {
	srv, lg := dashServer(t, testCookie)
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// sem cookie: recusado pelo servidor
	t0, err := c.Add(srv.URL+"/aula.mpd", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, st, t0.ID, domain.StateFailed)

	tk, err := c.AddWith(srv.URL+"/aula.mpd", domain.DownloadPolicy{}, Hints{Cookie: testCookie})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)
	n := 0
	for _, s := range lg.all() {
		if strings.HasSuffix(strings.SplitN(s, "|", 2)[0], ".m4s") || strings.HasSuffix(strings.SplitN(s, "|", 2)[0], ".mpd") {
			if strings.HasSuffix(s, "|"+testCookie) {
				n++
			}
		}
	}
	if n < 1+2+3+3 {
		t.Errorf("manifesto + 2 inits + 6 segmentos deviam levar o cookie; só %d levaram: %v", n, lg.all())
	}
}

func TestDASHManifestOnPrivateNetworkIsBlockedByDefault(t *testing.T) {
	srv, lg := dashServer(t, "")
	cfg := testCfg(t.TempDir())
	cfg.Security.AllowPrivateNetworks = false
	c, err := New(cfg, NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Add(srv.URL+"/aula.mpd", domain.DownloadPolicy{})
	de, ok := err.(*domain.DownloadError)
	if !ok || de.Class != domain.ErrPolicyViolation {
		t.Fatalf("esperava ErrPolicyViolation, obtido %#v", err)
	}
	if len(lg.all()) != 0 {
		t.Errorf("origem bloqueada não pode ser contactada: %v", lg.all())
	}
}
