package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"streamvault/core/app"
	"streamvault/core/config"
	"streamvault/core/license"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func memOpen(cfg config.Config, _ string, opts ...app.Option) (*app.Core, error) {
	return app.New(cfg, app.NewMemStore(), opts...)
}

func TestDaemonServesAuthenticatedAPIAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-data", t.TempDir(), "-dir", t.TempDir()}, out, memOpen)
	}()
	re := regexp.MustCompile(`Abra: (http://127\.0\.0\.1:\d+)/#token=([0-9a-f]{48})`)
	var m []string
	for i := 0; i < 200 && m == nil; i++ {
		m = re.FindStringSubmatch(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if m == nil {
		t.Fatalf("endereço não impresso: %q", out.String())
	}
	base, token := m[1], m[2]
	if resp, _ := http.Get(base + "/v1/health"); resp == nil || resp.StatusCode != 401 {
		t.Fatalf("sem token deve dar 401")
	}
	req, _ := http.NewRequest("GET", base+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health autenticado: %v %v", resp, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("o daemon não terminou")
	}
}

func TestDaemonRefusesNonLoopbackAndShowsVersion(t *testing.T) {
	if err := run(context.Background(), []string{"-addr", "0.0.0.0:0"}, &bytes.Buffer{}, memOpen); err == nil {
		t.Fatal("endereço não-loopback deve ser recusado")
	}
	var b bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &b, memOpen); err != nil || !strings.HasPrefix(b.String(), "streamvault ") {
		t.Fatalf("-version: %q %v", b.String(), err)
	}
}

func startDaemon(t *testing.T, args ...string) (base, token string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, out, memOpen) }()
	re := regexp.MustCompile(`Abra: (http://127\.0\.0\.1:\d+)/#token=([0-9a-f]{48})`)
	var m []string
	for i := 0; i < 300 && m == nil; i++ {
		m = re.FindStringSubmatch(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if m == nil {
		t.Fatalf("daemon não arrancou: %q", out.String())
	}
	return m[1], m[2], func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("daemon não terminou")
		}
	}
}

func post(t *testing.T, base, token, path string) int {
	req, _ := http.NewRequest("POST", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// LAN só abre com licença VÁLIDA que inclua LANTransfer.
func TestLANPairingNeedsValidLicenseWithLAN(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubFile := filepath.Join(dir, "k.pub")
	os.WriteFile(pubFile, []byte(hex.EncodeToString(pub)), 0o644)
	issue := func(name string, lanRight bool, product string) string {
		l := &license.License{FormatVersion: license.FormatVersion, LicenseID: name, ProductID: product,
			Edition: "pro", IssuedAt: time.Now().Add(-time.Hour).Unix(), KeyID: "k1",
			Entitlements:     license.Entitlements{LANTransfer: lanRight},
			ActivationPolicy: license.ActivationPolicy{MaxDevices: 1}}
		if err := l.Sign(priv); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(l)
		p := filepath.Join(dir, name+".json")
		os.WriteFile(p, b, 0o644)
		return p
	}
	base := []string{"-data", t.TempDir(), "-dir", t.TempDir(), "-lan-listen", "127.0.0.1:0", "-license-pub", pubFile, "-license-key-id", "k1"}

	cases := []struct {
		name string
		lic  string
		want int
	}{
		{"sem licença", "", 409},
		{"licença sem LAN", issue("nolan", false, "streamvault-windows"), 409},
		{"licença de outro produto", issue("other", true, "outro-produto"), 409},
		{"licença válida com LAN", issue("ok", true, "streamvault-windows"), 200},
	}
	for _, c := range cases {
		args := append([]string{}, base...)
		if c.lic != "" {
			args = append(args, "-license", c.lic)
		}
		b, tk, stop := startDaemon(t, args...)
		if got := post(t, b, tk, "/v1/lan/pairing"); got != c.want {
			t.Errorf("%s: pareamento devolveu %d (esperado %d)", c.name, got, c.want)
		}
		stop()
	}
}

// A chave de acesso persiste entre arranques (a extensão não precisa de reconfiguração)
// e só muda com -new-token.
func TestAccessTokenPersistsAcrossRestartsUntilRotated(t *testing.T) {
	data, dl := t.TempDir(), t.TempDir()
	tokenOf := func(extra ...string) string {
		_, tk, stop := startDaemon(t, append([]string{"-data", data, "-dir", dl, "-no-torrent"}, extra...)...)
		stop()
		return tk
	}
	a := tokenOf()
	if b := tokenOf(); a != b {
		t.Fatalf("a chave devia persistir: %s != %s", a, b)
	}
	fi, err := os.Stat(filepath.Join(data, "api-token"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("api-token deve existir com permissão 0600: %v %v", err, fi)
	}
	if c := tokenOf("-new-token"); c == a {
		t.Fatal("-new-token devia gerar uma chave nova")
	}
	// ficheiro corrompido: gera uma nova em vez de aceitar lixo
	os.WriteFile(filepath.Join(data, "api-token"), []byte("lixo"), 0o600)
	if d := tokenOf(); len(d) != 48 {
		t.Fatalf("chave inválida devia ser substituída: %q", d)
	}
}

// Porta por defeito ocupada: o daemon arranca noutra em vez de falhar.
func TestDaemonFallsBackWhenDefaultPortIsBusy(t *testing.T) {
	busy, err := net.Listen("tcp", defaultAddr)
	if err != nil {
		t.Skip("porta por defeito indisponível neste ambiente")
	}
	defer busy.Close()
	base, _, stop := startDaemon(t, "-data", t.TempDir(), "-dir", t.TempDir(), "-no-torrent")
	defer stop()
	if strings.HasSuffix(base, ":47800") {
		t.Fatalf("devia ter usado outra porta, usou %s", base)
	}
}
