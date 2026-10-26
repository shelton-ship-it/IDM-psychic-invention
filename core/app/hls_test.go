package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"streamvault/core/domain"
	"streamvault/core/storage"
)

// hlsFixture serve um VOD HLS sem cifra (3 segmentos .ts relativos) e regista o
// Referer / User-Agent de cada pedido, para provar que as pistas do navegador
// chegam à playlist E aos segmentos.
type hlsFixture struct {
	*httptest.Server
	segs [][]byte

	mu   sync.Mutex
	reqs []hlsReq
}

type hlsReq struct{ path, referer, ua string }

func newHLSFixture(t *testing.T, playlistExtra string) *hlsFixture {
	t.Helper()
	f := &hlsFixture{segs: [][]byte{payloadOf(40 * 1024), payloadOf(70 * 1024), payloadOf(25 * 1024)}}
	f.segs[1][0] ^= 0xFF // segmentos distintos: a concatenação só bate se a ordem estiver certa
	f.segs[2][0] ^= 0x55
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, hlsReq{r.URL.Path, r.Header.Get("Referer"), r.Header.Get("User-Agent")})
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/v.m3u8":
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-PLAYLIST-TYPE:VOD\n")
			b.WriteString(playlistExtra)
			for i := range f.segs {
				fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(b.String()))
		case strings.HasPrefix(r.URL.Path, "/seg") && strings.HasSuffix(r.URL.Path, ".ts"):
			var i int
			if _, err := fmt.Sscanf(r.URL.Path, "/seg%d.ts", &i); err != nil || i < 0 || i >= len(f.segs) {
				http.NotFound(w, r)
				return
			}
			w.Write(f.segs[i])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *hlsFixture) requests() []hlsReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hlsReq(nil), f.reqs...)
}

// Ponta a ponta: Add(.m3u8) → adaptador HLS (antes do HTTP) → motor HLS real →
// Completed + ficheiro concatenado na ordem certa + índice de hash.
func TestEndToEndHLSDownload(t *testing.T) {
	fx := newHLSFixture(t, "")
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const referer, ua = "https://pagina.example.com/aula/1", "Mozilla/5.0 (teste)"
	tk, err := c.AddWith(fx.URL+"/v.m3u8", domain.DownloadPolicy{}, Hints{Referer: referer, UserAgent: ua})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Source.Kind != domain.SourceHLS {
		t.Fatalf("o .m3u8 deve ser reconhecido como HLS (adaptador antes do HTTP), obtido %q", tk.Source.Kind)
	}
	waitState(t, st, tk.ID, domain.StateCompleted)

	want := bytes.Join(fx.segs, nil)
	got, err := os.ReadFile(filepath.Join(dir, "v.ts"))
	if err != nil {
		ents, _ := os.ReadDir(dir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("ficheiro final em falta: %v (pasta: %v)", err, names)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("conteúdo diverge: %d bytes, esperados %d", len(got), len(want))
	}

	// pistas do navegador: em TODOS os pedidos (playlist + segmentos)
	reqs := fx.requests()
	if len(reqs) < 1+len(fx.segs) {
		t.Fatalf("esperava playlist + %d segmentos, vi %d pedidos: %+v", len(fx.segs), len(reqs), reqs)
	}
	for _, r := range reqs {
		if r.referer != referer || r.ua != ua {
			t.Errorf("pedido %s sem as pistas do navegador: referer=%q ua=%q", r.path, r.referer, r.ua)
		}
	}

	// índice de hashes (duplicados exatos): só depois de concluída+verificada
	sum := sha256.Sum256(want)
	var e storage.HashEntry
	var ok bool
	for i := 0; i < 200 && !ok; i++ {
		st.mu.Lock()
		e, ok = st.hashes[hex.EncodeToString(sum[:])]
		st.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || e.SourceTask != tk.ID {
		t.Fatalf("o índice de hashes devia ter o ficheiro HLS concluído: ok=%v %+v", ok, e)
	}
}

// Stream cifrado: falha explicada (unsupported_source), nunca um ficheiro lixo.
func TestHLSEncryptedStreamFailsHonestly(t *testing.T) {
	fx := newHLSFixture(t, "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n")
	dir := t.TempDir()
	st := NewMemStore()
	c, err := New(testCfg(dir), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tk, err := c.Add(fx.URL+"/v.m3u8", domain.DownloadPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitState(t, st, tk.ID, domain.StateFailed)
	if got.Error == nil || got.Error.Class != domain.ErrUnsupportedSource {
		t.Fatalf("esperava failed/unsupported_source, obtido %+v", got.Error)
	}
	if got.Error.Message == "" || got.Error.Action == "" {
		t.Errorf("a falha deve explicar o motivo e o que fazer: %+v", got.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "v.ts")); err == nil {
		t.Error("não podia ficar um ficheiro final de um stream cifrado")
	}
	for _, r := range fx.requests() {
		if strings.HasPrefix(r.path, "/seg") || r.path == "/key.bin" {
			t.Errorf("não devia descarregar segmentos/chaves de um stream cifrado: %s", r.path)
		}
	}
}

// A verificação de segurança do Add cobre HLS (antes só corria para HTTP): uma
// playlist em rede privada é recusada, sem sequer contactar o servidor.
func TestHLSPlaylistOnPrivateNetworkIsBlockedByDefault(t *testing.T) {
	fx := newHLSFixture(t, "")
	cfg := testCfg(t.TempDir())
	cfg.Security.AllowPrivateNetworks = false
	st := NewMemStore()
	c, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.Add(fx.URL+"/v.m3u8", domain.DownloadPolicy{})
	de, ok := err.(*domain.DownloadError)
	if !ok || de.Class != domain.ErrPolicyViolation {
		t.Fatalf("esperava ErrPolicyViolation, obtido %#v", err)
	}
	if n := len(fx.requests()); n != 0 {
		t.Errorf("uma origem bloqueada não pode ser contactada; pedidos: %d", n)
	}
}
