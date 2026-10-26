// Package lan implementa o handoff entre dispositivos na mesma rede (Fase 10,
// ADR-004): descoberta por UDP, pareamento por PIN, canal cifrado e receção
// segura de links e ficheiros. Só usa a biblioteca padrão.
//
// Modelo de ameaça: a LAN é hostil (qualquer dispositivo pode ligar-se). Por
// isso: (1) nada é aceite sem PIN de uso único mostrado no recetor; (2) o PIN
// está LIGADO às chaves de sessão (um MITM ativo tem de adivinhar o PIN em
// tentativas online limitadas; um observador passivo não consegue testar PINs
// offline porque o MAC depende do segredo Diffie-Hellman); (3) todo o tráfego
// após o pareamento é AES-256-GCM com contador; (4) o recetor valida nome,
// tamanho e SHA-256 e nunca sobrescreve nem executa nada.
package lan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// hkdf implementa HKDF-SHA256 (RFC 5869).
func hkdf(secret, salt, info []byte, n int) []byte {
	ext := hmac.New(sha256.New, salt)
	ext.Write(secret)
	prk := ext.Sum(nil)
	var out, t []byte
	for i := byte(1); len(out) < n; i++ {
		h := hmac.New(sha256.New, prk)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{i})
		t = h.Sum(nil)
		out = append(out, t...)
	}
	return out[:n]
}

const (
	maxPayload = 64 << 10 // por frame
	gcmTag     = 16
)

// secureConn é um canal de frames autenticados e cifrados. O nonce é um
// contador por direção (chaves distintas por direção ⇒ sem reutilização);
// reordenar, repetir ou descartar frames faz a autenticação falhar.
type secureConn struct {
	c        net.Conn
	enc, dec cipher.AEAD
	sctr     uint64
	rctr     uint64
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func nonce(ctr uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], ctr)
	return n
}

func (s *secureConn) write(typ byte, payload []byte) error {
	if len(payload) > maxPayload {
		return fmt.Errorf("lan: payload demasiado grande (%d)", len(payload))
	}
	plain := append([]byte{typ}, payload...)
	ct := s.enc.Seal(nil, nonce(s.sctr), plain, nil)
	s.sctr++
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(ct)))
	_, err := s.c.Write(append(hdr, ct...))
	return err
}

func (s *secureConn) read() (byte, []byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(s.c, hdr); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr)
	if n < 1+gcmTag || n > maxPayload+1+gcmTag {
		return 0, nil, errors.New("lan: tamanho de frame inválido")
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(s.c, ct); err != nil {
		return 0, nil, err
	}
	plain, err := s.dec.Open(nil, nonce(s.rctr), ct, nil)
	if err != nil {
		return 0, nil, errors.New("lan: frame não autenticado")
	}
	s.rctr++
	return plain[0], plain[1:], nil
}
