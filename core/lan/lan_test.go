package lan

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// RFC 5869, Apêndice A.1
func TestHKDFRFC5869Vector(t *testing.T) {
	ikm, _ := hex.DecodeString("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	salt, _ := hex.DecodeString("000102030405060708090a0b0c")
	info, _ := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	want := "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"
	if got := hex.EncodeToString(hkdf(ikm, salt, info, 42)); got != want {
		t.Fatalf("HKDF diverge do vetor RFC 5869:\n got %s\nwant %s", got, want)
	}
}

type rig struct {
	t     *testing.T
	r     *Receiver
	addr  string
	dir   string
	mu    sync.Mutex
	links []string
	files []string
}

func newRig(t *testing.T, mod func(*ReceiverOptions)) *rig {
	t.Helper()
	g := &rig{t: t, dir: t.TempDir()}
	o := ReceiverOptions{Name: "PC", Dir: g.dir, MaxFileSize: 10 << 20,
		OnLink:         func(peer, link string) bool { g.mu.Lock(); g.links = append(g.links, link); g.mu.Unlock(); return true },
		OnFile:         func(peer, name string, size int64) bool { return true },
		OnFileReceived: func(peer, path string) { g.mu.Lock(); g.files = append(g.files, path); g.mu.Unlock() }}
	if mod != nil {
		mod(&o)
	}
	r, err := NewReceiver(o)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go r.Serve(ln)
	t.Cleanup(func() { r.Close() })
	g.r, g.addr = r, ln.Addr().String()
	return g
}

func (g *rig) connect(pin string) (*Session, error) {
	return Connect(context.Background(), g.addr, pin, SendOptions{Name: "telemóvel"})
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

func TestLinkHandoffAndRejection(t *testing.T) {
	g := newRig(t, nil)
	pin, _ := g.r.StartPairing()
	s, err := g.connect(pin)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SendLink("https://example.com/a.bin"); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	if len(g.links) != 1 || g.links[0] != "https://example.com/a.bin" {
		t.Fatalf("link não chegou: %v", g.links)
	}
	g.mu.Unlock()
	// vários itens na mesma sessão pareada
	if err := s.SendLink("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}

	g2 := newRig(t, func(o *ReceiverOptions) { o.OnLink = func(string, string) bool { return false } })
	pin2, _ := g2.r.StartPairing()
	s2, err := g2.connect(pin2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var rej *RejectedError
	if err := s2.SendLink("https://x/y"); !errors.As(err, &rej) {
		t.Fatalf("esperava RejectedError, obtido %v", err)
	}
	if err := s2.SendLink(strings.Repeat("a", 5000)); !errors.As(err, &rej) {
		t.Fatalf("link gigante deveria ser recusado: %v", err)
	}
}

func TestFileTransferVerifiedAndNeverOverwrites(t *testing.T) {
	g := newRig(t, nil)
	src := filepath.Join(t.TempDir(), "relatorio.pdf")
	data := payload(300_000) // vários frames
	os.WriteFile(src, data, 0o644)
	// já existe um ficheiro com o mesmo nome
	os.WriteFile(filepath.Join(g.dir, "relatorio.pdf"), []byte("antigo"), 0o644)

	pin, _ := g.r.StartPairing()
	s, err := g.connect(pin)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SendFile(src); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(g.dir, "relatorio.pdf")); string(b) != "antigo" {
		t.Fatal("sobrescreveu o ficheiro existente")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.files) != 1 || filepath.Base(g.files[0]) != "relatorio (1).pdf" {
		t.Fatalf("destino inesperado: %v", g.files)
	}
	if got, _ := os.ReadFile(g.files[0]); !bytes.Equal(got, data) {
		t.Fatal("conteúdo recebido diverge")
	}
	if es, _ := filepath.Glob(filepath.Join(g.dir, "*.svpart")); len(es) != 0 {
		t.Errorf("ficheiro temporário ficou: %v", es)
	}
}

func TestWrongPinExhaustedPinAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	g := newRig(t, func(o *ReceiverOptions) { o.Now = clock })

	// sem janela de pareamento ativa
	if _, err := g.connect("000000"); !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("sem pareamento ativo: %v", err)
	}
	pin, _ := g.r.StartPairing()
	wrong := "000000"
	if pin == wrong {
		wrong = "111111"
	}
	for i := 0; i < pinAttempts; i++ {
		if _, err := g.connect(wrong); !errors.Is(err, ErrPairingFailed) {
			t.Fatalf("PIN errado #%d: %v", i, err)
		}
	}
	// tentativas esgotadas: nem o PIN certo funciona (impede força bruta)
	if _, err := g.connect(pin); !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("PIN certo após esgotar tentativas deveria falhar: %v", err)
	}
	// nova janela funciona
	pin, _ = g.r.StartPairing()
	s, err := g.connect(pin)
	if err != nil {
		t.Fatalf("novo pareamento: %v", err)
	}
	s.Close()
	// PIN de uso único
	if _, err := g.connect(pin); !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("reutilizar o PIN deveria falhar: %v", err)
	}
	// expiração
	pin, _ = g.r.StartPairing()
	mu.Lock()
	now = now.Add(pinTTL + time.Second)
	mu.Unlock()
	if _, err := g.connect(pin); !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("PIN expirado deveria falhar: %v", err)
	}
}

// Um intermediário que adultera o handshake não consegue pareamento.
func TestTamperedHandshakeIsRejected(t *testing.T) {
	g := newRig(t, nil)
	pin, _ := g.r.StartPairing()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		up, err := net.Dial("tcp", g.addr)
		if err != nil {
			return
		}
		defer up.Close()
		go func() { // cliente -> recetor: intacto
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					up.Write(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		first := true
		buf := make([]byte, 4096)
		for { // recetor -> cliente: altera 1 byte da chave pública
			n, err := up.Read(buf)
			if n > 0 {
				if first && n > 5 {
					buf[5] ^= 0xFF
					first = false
				}
				c.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	_, err := Connect(context.Background(), ln.Addr().String(), pin, SendOptions{Name: "x"})
	if !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("handshake adulterado deveria falhar: %v", err)
	}
}

func TestFileOfferValidation(t *testing.T) {
	g := newRig(t, func(o *ReceiverOptions) { o.MaxFileSize = 1000 })
	pin, _ := g.r.StartPairing()
	s, err := g.connect(pin)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sum := hex.EncodeToString(make([]byte, 32))
	var rej *RejectedError
	for _, name := range []string{"../evil.txt", "..", "/etc/passwd", "a/b.txt", `C:\x.txt`, "", "x\x00y"} {
		if err := s.offer(name, 10, sum); !errors.As(err, &rej) {
			t.Errorf("nome %q deveria ser recusado: %v", name, err)
		}
	}
	if err := s.offer("grande.bin", 5000, sum); !errors.As(err, &rej) {
		t.Errorf("acima do limite deveria ser recusado: %v", err)
	}
	if err := s.offer("ok.bin", 10, "nao-e-hex"); !errors.As(err, &rej) {
		t.Errorf("hash inválido deveria ser recusado: %v", err)
	}
	if es, _ := os.ReadDir(g.dir); len(es) != 0 {
		t.Errorf("recusas não podem deixar rasto em disco: %v", es)
	}
}

func TestFileWithoutUserConfirmationIsRefused(t *testing.T) {
	g := newRig(t, func(o *ReceiverOptions) { o.OnFile = nil })
	pin, _ := g.r.StartPairing()
	s, _ := g.connect(pin)
	defer s.Close()
	src := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(src, []byte("x"), 0o644)
	var rej *RejectedError
	if err := s.SendFile(src); !errors.As(err, &rej) {
		t.Fatalf("sem OnFile deve recusar: %v", err)
	}
}

func TestHashMismatchAndOversendLeaveNoFiles(t *testing.T) {
	g := newRig(t, nil)
	pin, _ := g.r.StartPairing()
	s, err := g.connect(pin)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := payload(5000)
	lie := sha256.Sum256([]byte("outro conteúdo"))
	if err := s.offer("a.bin", int64(len(data)), hex.EncodeToString(lie[:])); err != nil {
		t.Fatal(err)
	}
	var rej *RejectedError
	if err := s.stream(bytes.NewReader(data)); !errors.As(err, &rej) {
		t.Fatalf("hash falso deveria falhar a verificação: %v", err)
	}
	if es, _ := os.ReadDir(g.dir); len(es) != 0 {
		t.Fatalf("hash falso deixou ficheiros: %v", es)
	}

	// enviar MAIS bytes do que o declarado aborta a sessão e não deixa rasto
	pin, _ = g.r.StartPairing()
	s2, err := g.connect(pin)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	sum := sha256.Sum256(data[:100])
	if err := s2.offer("b.bin", 100, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := s2.stream(bytes.NewReader(data)); err == nil {
		t.Fatal("excesso de bytes deveria falhar")
	}
	time.Sleep(100 * time.Millisecond)
	if es, _ := os.ReadDir(g.dir); len(es) != 0 {
		t.Fatalf("excesso de bytes deixou ficheiros: %v", es)
	}
}

func TestLicenseGateBlocksBothSides(t *testing.T) {
	g := newRig(t, nil)
	pin, _ := g.r.StartPairing()
	if _, err := Connect(context.Background(), g.addr, pin, SendOptions{Gate: func() bool { return false }}); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("emissor sem licença: %v", err)
	}
	g2 := newRig(t, func(o *ReceiverOptions) { o.Gate = func() bool { return false } })
	pin2, _ := g2.r.StartPairing()
	if _, err := g2.connect(pin2); !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("recetor sem licença deve recusar: %v", err)
	}
}

func TestNewReceiverRequiresDir(t *testing.T) {
	if _, err := NewReceiver(ReceiverOptions{}); err == nil {
		t.Error("Dir obrigatório")
	}
}

func TestDiscoveryOverLoopback(t *testing.T) {
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	addr := pc.LocalAddr().String()
	pc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Peer, 4)
	go Browse(ctx, addr, func(p Peer) { got <- p })
	time.Sleep(100 * time.Millisecond)
	go Beacon(ctx, addr, Announcement{ID: "dev-1", Name: "Sala\x07", Port: 5555}, 50*time.Millisecond)
	select {
	case p := <-got:
		if p.ID != "dev-1" || p.Addr != "127.0.0.1:5555" || strings.ContainsRune(p.Name, 7) {
			t.Fatalf("peer inesperado: %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("anúncio não recebido")
	}
	// lixo/pacotes inválidos são ignorados sem derrubar o Browse
	raddr, _ := net.ResolveUDPAddr("udp", addr)
	c, _ := net.DialUDP("udp", nil, raddr)
	c.Write([]byte("lixo"))
	c.Write([]byte(`{"v":1,"id":"x","port":0}`))
	c.Close()
}

// Cliente malicioso: PIN errado e NÃO verifica a prova do servidor. O recetor
// tem de o recusar por si (autenticação do cliente, não só do servidor).
func TestReceiverAuthenticatesClientIndependently(t *testing.T) {
	g := newRig(t, nil)
	pin, _ := g.r.StartPairing()
	wrong := "000000"
	if pin == wrong {
		wrong = "111111"
	}
	c, err := net.Dial("tcp", g.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	nonceC := make([]byte, 16)
	rand.Read(nonceC)
	rawC := encodeHello(true, priv.PublicKey().Bytes(), nonceC, "intruso")
	c.Write(rawC)
	rawS, pubSb, nonceS, _, err := readHello(c, false)
	if err != nil {
		t.Fatal(err)
	}
	pubS, _ := ecdh.X25519().NewPublicKey(pubSb)
	shared, _ := priv.ECDH(pubS)
	keys := deriveKeys(shared, nonceC, nonceS, wrong)
	transcript := append(append([]byte{}, rawC...), rawS...)
	c.Write(keys.confirm("client", transcript))
	// ignora a prova do servidor e tenta usar o canal na mesma
	sc, _ := newSecure(c, keys, true)
	b, _ := json.Marshal(linkOffer{Link: "https://intruso.example/x"})
	sc.write(msgLink, b)
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err == nil && n > 0 {
		// a única coisa que pode chegar é nada: o recetor fecha sem responder
		t.Fatalf("recetor respondeu a um cliente sem PIN válido (%d bytes)", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.links) != 0 {
		t.Fatalf("OnLink chamado para cliente sem PIN: %v", g.links)
	}
}
