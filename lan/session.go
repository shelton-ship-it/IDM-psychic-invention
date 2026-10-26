package lan

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"
)

var magic = []byte("SVLAN1")

const (
	handshakeTimeout = 10 * time.Second
	idleTimeout      = 60 * time.Second
	maxNameLen       = 64
	pinTTL           = 2 * time.Minute
	pinAttempts      = 3
)

// Erros públicos. ErrPairingFailed é deliberadamente genérico: não diz se o
// PIN estava errado, expirado ou esgotado.
var (
	ErrPairingFailed = errors.New("lan: pareamento falhou (PIN errado, expirado ou esgotado)")
	ErrNotEntitled   = errors.New("lan: a transferência LAN não está incluída na licença")
)

// RejectedError: o outro lado recusou a oferta.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "lan: recusado pelo outro dispositivo: " + e.Reason }

func cleanName(n string) string {
	b := []rune{}
	for _, r := range n {
		if r >= 32 && r != 127 && len(b) < maxNameLen {
			b = append(b, r)
		}
	}
	if len(b) == 0 {
		return "dispositivo"
	}
	return string(b)
}

// hello: magic(6) | pub(32) | nonce(16) | nameLen(1) | name — o cliente
// envia magic; o servidor responde sem magic.
func encodeHello(withMagic bool, pub, nonce []byte, name string) []byte {
	name = cleanName(name)
	var b []byte
	if withMagic {
		b = append(b, magic...)
	}
	b = append(b, pub...)
	b = append(b, nonce...)
	b = append(b, byte(len(name)))
	return append(b, name...)
}

func readHello(c net.Conn, withMagic bool) (raw, pub, nonce []byte, name string, err error) {
	head := 32 + 16 + 1
	if withMagic {
		head += len(magic)
	}
	buf := make([]byte, head)
	if _, err = io.ReadFull(c, buf); err != nil {
		return
	}
	off := 0
	if withMagic {
		if string(buf[:len(magic)]) != string(magic) {
			err = errors.New("lan: protocolo desconhecido")
			return
		}
		off = len(magic)
	}
	pub, nonce = buf[off:off+32], buf[off+32:off+48]
	nl := int(buf[off+48])
	if nl > maxNameLen {
		err = errors.New("lan: nome demasiado longo")
		return
	}
	nb := make([]byte, nl)
	if _, err = io.ReadFull(c, nb); err != nil {
		return
	}
	return append(buf, nb...), pub, nonce, string(nb), nil
}

type sessionKeys struct {
	c2s, s2c, pinKey []byte
}

func deriveKeys(shared, nonceC, nonceS []byte, pin string) sessionKeys {
	salt := append(append([]byte{}, nonceC...), nonceS...)
	km := hkdf(shared, salt, []byte("streamvault-lan/v1/keys"), 64)
	return sessionKeys{c2s: km[:32], s2c: km[32:], pinKey: hkdf([]byte(pin), salt, []byte("streamvault-lan/v1/pin"), 32)}
}

// confirm liga o PIN às chaves de sessão e ao transcript completo.
func (k sessionKeys) confirm(label string, transcript []byte) []byte {
	m := hmac.New(sha256.New, k.pinKey)
	m.Write([]byte(label))
	m.Write(transcript)
	m.Write(k.c2s)
	m.Write(k.s2c)
	return m.Sum(nil)
}

func newSecure(c net.Conn, k sessionKeys, client bool) (*secureConn, error) {
	send, recv := k.s2c, k.c2s
	if client {
		send, recv = k.c2s, k.s2c
	}
	enc, err := newAEAD(send)
	if err != nil {
		return nil, err
	}
	dec, err := newAEAD(recv)
	if err != nil {
		return nil, err
	}
	return &secureConn{c: c, enc: enc, dec: dec}, nil
}

// ---------------------------------------------------------------------------
// Recetor
// ---------------------------------------------------------------------------

// Mensagens.
const (
	msgLink      byte = 1
	msgFileOffer byte = 2
	msgData      byte = 3
	msgEnd       byte = 4
	msgAccept    byte = 0x10
	msgReject    byte = 0x11
	msgDone      byte = 0x12
	msgFail      byte = 0x13
)

// ReceiverOptions configura o recetor.
type ReceiverOptions struct {
	Name        string
	Dir         string // pasta onde os ficheiros recebidos ficam (obrigatória)
	MaxFileSize int64  // 0 => 4 GiB
	// OnLink decide se um link oferecido é aceite (ex.: enfileirar no núcleo).
	OnLink func(peer, link string) bool
	// OnFile decide se um ficheiro oferecido é aceite (a UI mostra nome/tamanho).
	OnFile func(peer, name string, size int64) bool
	// OnFileReceived é chamado após receção verificada.
	OnFileReceived func(peer, path string)
	// Gate devolve false se a licença não inclui LAN (ver license.Entitlements.LANTransfer).
	Gate func() bool
	Now  func() time.Time // injetável em testes
}

// Receiver aceita ofertas de dispositivos pareados.
type Receiver struct {
	o  ReceiverOptions
	mu sync.Mutex
	pairing
	ln     net.Listener
	closed bool
}

type pairing struct {
	pin      string
	exp      time.Time
	attempts int
}

// NewReceiver valida as opções.
func NewReceiver(o ReceiverOptions) (*Receiver, error) {
	if o.Dir == "" {
		return nil, errors.New("lan: Dir obrigatório")
	}
	if o.MaxFileSize <= 0 {
		o.MaxFileSize = 4 << 30
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Name == "" {
		o.Name = "StreamVault"
	}
	return &Receiver{o: o}, nil
}

// StartPairing abre uma janela de pareamento: devolve um PIN de 6 dígitos
// (uso único, 2 min, 3 tentativas) a mostrar NO ECRÃ do recetor.
func (r *Receiver) StartPairing() (pin string, expires time.Time) {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	pin = fmt.Sprintf("%06d", n.Int64())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairing = pairing{pin: pin, exp: r.o.Now().Add(pinTTL), attempts: pinAttempts}
	return pin, r.pairing.exp
}

// takeAttempt reserva uma tentativa (devolve o PIN ativo) ou falha.
func (r *Receiver) takeAttempt() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pin == "" || r.attempts <= 0 || !r.o.Now().Before(r.exp) {
		r.pairing = pairing{}
		return "", false
	}
	r.attempts--
	return r.pin, true
}

func (r *Receiver) consume() {
	r.mu.Lock()
	r.pairing = pairing{}
	r.mu.Unlock()
}

// Serve aceita ligações até Close. Bloqueia.
func (r *Receiver) Serve(ln net.Listener) error {
	r.mu.Lock()
	r.ln = ln
	r.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go r.handle(c)
	}
}

// Close para o recetor.
func (r *Receiver) Close() error {
	r.mu.Lock()
	r.closed = true
	ln := r.ln
	r.mu.Unlock()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

func (r *Receiver) handle(c net.Conn) {
	defer c.Close()
	if r.o.Gate != nil && !r.o.Gate() {
		return
	}
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	rawC, pubCb, nonceC, peerName, err := readHello(c, true)
	if err != nil {
		return
	}
	pin, ok := r.takeAttempt()
	if !ok {
		return // sem janela ativa: fecha sem revelar nada
	}
	pubC, err := ecdh.X25519().NewPublicKey(pubCb)
	if err != nil {
		return
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	nonceS := make([]byte, 16)
	rand.Read(nonceS)
	rawS := encodeHello(false, priv.PublicKey().Bytes(), nonceS, r.o.Name)
	if _, err := c.Write(rawS); err != nil {
		return
	}
	shared, err := priv.ECDH(pubC)
	if err != nil {
		return
	}
	keys := deriveKeys(shared, nonceC, nonceS, pin)
	transcript := append(append([]byte{}, rawC...), rawS...)
	got := make([]byte, 32)
	if _, err := io.ReadFull(c, got); err != nil {
		return
	}
	if !hmac.Equal(got, keys.confirm("client", transcript)) {
		return // PIN errado ou handshake adulterado
	}
	r.consume() // PIN de uso único
	if _, err := c.Write(keys.confirm("server", transcript)); err != nil {
		return
	}
	sc, err := newSecure(c, keys, false)
	if err != nil {
		return
	}
	r.session(sc, c, cleanName(peerName))
}

func (r *Receiver) session(sc *secureConn, c net.Conn, peer string) {
	for {
		c.SetDeadline(time.Now().Add(idleTimeout))
		typ, p, err := sc.read()
		if err != nil {
			return
		}
		switch typ {
		case msgLink:
			r.onLink(sc, peer, p)
		case msgFileOffer:
			if !r.onFile(sc, c, peer, p) {
				return
			}
		default:
			return
		}
	}
}

func reject(sc *secureConn, reason string) { _ = sc.write(msgReject, []byte(reason)) }
