package lan

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// SendOptions configura o lado que envia.
type SendOptions struct {
	Name string // nome deste dispositivo, mostrado no recetor
	Gate func() bool
}

// Session é uma sessão já pareada com um recetor.
type Session struct {
	sc *secureConn
	c  net.Conn
}

// Connect liga-se a addr (host:porta), prova o PIN e devolve a sessão cifrada.
// Qualquer falha de pareamento devolve o mesmo ErrPairingFailed.
func Connect(ctx context.Context, addr, pin string, o SendOptions) (*Session, error) {
	if o.Gate != nil && !o.Gate() {
		return nil, ErrNotEntitled
	}
	d := net.Dialer{Timeout: handshakeTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	fail := func() (*Session, error) { c.Close(); return nil, ErrPairingFailed }
	c.SetDeadline(time.Now().Add(handshakeTimeout))

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fail()
	}
	nonceC := make([]byte, 16)
	rand.Read(nonceC)
	rawC := encodeHello(true, priv.PublicKey().Bytes(), nonceC, o.Name)
	if _, err := c.Write(rawC); err != nil {
		return fail()
	}
	rawS, pubSb, nonceS, _, err := readHello(c, false)
	if err != nil {
		return fail()
	}
	pubS, err := ecdh.X25519().NewPublicKey(pubSb)
	if err != nil {
		return fail()
	}
	shared, err := priv.ECDH(pubS)
	if err != nil {
		return fail()
	}
	keys := deriveKeys(shared, nonceC, nonceS, pin)
	transcript := append(append([]byte{}, rawC...), rawS...)
	if _, err := c.Write(keys.confirm("client", transcript)); err != nil {
		return fail()
	}
	got := make([]byte, 32)
	if _, err := io.ReadFull(c, got); err != nil {
		return fail() // o recetor fecha quando o PIN está errado
	}
	if !hmac.Equal(got, keys.confirm("server", transcript)) {
		return fail()
	}
	sc, err := newSecure(c, keys, true)
	if err != nil {
		return fail()
	}
	return &Session{sc: sc, c: c}, nil
}

// Close termina a sessão.
func (s *Session) Close() error { return s.c.Close() }

func (s *Session) reply() error {
	s.c.SetDeadline(time.Now().Add(idleTimeout))
	typ, p, err := s.sc.read()
	if err != nil {
		return err
	}
	switch typ {
	case msgAccept, msgDone:
		return nil
	case msgReject, msgFail:
		return &RejectedError{Reason: string(p)}
	}
	return errors.New("lan: resposta inesperada")
}

// SendLink oferece um link; o recetor decide se o enfileira.
func (s *Session) SendLink(link string) error {
	b, _ := json.Marshal(linkOffer{Link: link})
	s.c.SetDeadline(time.Now().Add(idleTimeout))
	if err := s.sc.write(msgLink, b); err != nil {
		return err
	}
	return s.reply()
}

// SendFile envia um ficheiro com verificação SHA-256 no recetor.
func (s *Session) SendFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := s.offer(filepath.Base(path), st.Size(), hex.EncodeToString(h.Sum(nil))); err != nil {
		return err
	}
	return s.stream(f)
}

func (s *Session) offer(name string, size int64, sum string) error {
	b, _ := json.Marshal(fileOffer{Name: name, Size: size, SHA256: sum})
	s.c.SetDeadline(time.Now().Add(idleTimeout))
	if err := s.sc.write(msgFileOffer, b); err != nil {
		return err
	}
	return s.reply() // aceitar/recusar
}

func (s *Session) stream(r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.c.SetDeadline(time.Now().Add(idleTimeout))
			if werr := s.sc.write(msgData, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	s.c.SetDeadline(time.Now().Add(idleTimeout))
	if err := s.sc.write(msgEnd, nil); err != nil {
		return err
	}
	return s.reply() // Done/Fail
}
