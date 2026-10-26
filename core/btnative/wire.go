// Package btnative é um motor BitTorrent em Go puro (só biblioteca padrão):
// protocolo de peers (BEP 3), extensões (BEP 10), metadados de magnet (BEP 9),
// trackers HTTP e UDP (BEP 3/15) e descoberta DHT (BEP 5, cliente). Implementa
// torrentengine.Backend; toda a verificação de integridade continua a ser feita
// pelo torrentengine (peças SHA-1) antes de um download ser dado como concluído.
//
// Fora de âmbito desta versão: uTP, cifra de protocolo (MSE/PE), PEX, DHT como
// servidor e IPv6 na DHT.
package btnative

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

const (
	pstr          = "BitTorrent protocol"
	blockSize     = 16 << 10
	maxMsgLen     = 1<<17 + 9 // maior mensagem aceite (bloco de 128 KiB + cabeçalho)
	maxBlockReq   = 1 << 17   // maior pedido de bloco que servimos
	metadataPiece = 16 << 10
)

// ids das mensagens.
const (
	msgChoke         byte = 0
	msgUnchoke       byte = 1
	msgInterested    byte = 2
	msgNotInterested byte = 3
	msgHave          byte = 4
	msgBitfield      byte = 5
	msgRequest       byte = 6
	msgPiece         byte = 7
	msgCancel        byte = 8
	msgExtended      byte = 20
)

// ids de extensão (BEP 10) e tipos ut_metadata (BEP 9).
const (
	extHandshake  byte = 0
	extUTMetadata byte = 1 // id LOCAL que anunciamos para ut_metadata

	mdRequest = 0
	mdData    = 1
	mdReject  = 2
)

// handshake: <19>"BitTorrent protocol"<8 reservados><info_hash 20><peer_id 20>
func encodeHandshake(infoHash, peerID [20]byte) []byte {
	b := make([]byte, 0, 68)
	b = append(b, byte(len(pstr)))
	b = append(b, pstr...)
	reserved := make([]byte, 8)
	reserved[5] |= 0x10 // suportamos o protocolo de extensões (BEP 10)
	b = append(b, reserved...)
	b = append(b, infoHash[:]...)
	return append(b, peerID[:]...)
}

type handshake struct {
	reserved [8]byte
	infoHash [20]byte
	peerID   [20]byte
}

func (h handshake) supportsExtensions() bool { return h.reserved[5]&0x10 != 0 }

// readHandshakePrefix lê pstrlen+pstr+reservados+info_hash (48+... bytes) sem o peer_id.
func readHandshakeHead(r io.Reader) (handshake, error) {
	var h handshake
	var l [1]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return h, err
	}
	if int(l[0]) != len(pstr) {
		return h, errors.New("btnative: protocolo desconhecido")
	}
	buf := make([]byte, len(pstr)+8+20)
	if _, err := io.ReadFull(r, buf); err != nil {
		return h, err
	}
	if string(buf[:len(pstr)]) != pstr {
		return h, errors.New("btnative: protocolo desconhecido")
	}
	copy(h.reserved[:], buf[len(pstr):len(pstr)+8])
	copy(h.infoHash[:], buf[len(pstr)+8:])
	return h, nil
}

func readPeerID(r io.Reader, h *handshake) error {
	_, err := io.ReadFull(r, h.peerID[:])
	return err
}

// writeMsg envia <len><id><payload>.
func writeMsg(w io.Writer, id byte, payload ...[]byte) error {
	n := 1
	for _, p := range payload {
		n += len(p)
	}
	buf := make([]byte, 4, 4+n)
	binary.BigEndian.PutUint32(buf, uint32(n))
	buf = append(buf, id)
	for _, p := range payload {
		buf = append(buf, p...)
	}
	_, err := w.Write(buf)
	return err
}

func writeKeepAlive(w io.Writer) error { _, err := w.Write([]byte{0, 0, 0, 0}); return err }

// readMsg lê uma mensagem; keepalive devolve id=255 e payload vazio. Mensagens
// acima de maxMsgLen são recusadas SEM alocar (defesa contra memória hostil).
func readMsg(r io.Reader) (id byte, payload []byte, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return 255, nil, nil
	}
	if n > maxMsgLen {
		return 0, nil, fmt.Errorf("btnative: mensagem de %d bytes excede o limite", n)
	}
	buf := make([]byte, n)
	if _, err = io.ReadFull(r, buf); err != nil {
		return
	}
	return buf[0], buf[1:], nil
}

func u32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func put32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// bitfield: bit i na posição MSB-first do byte i/8.
func encodeBitfield(have []bool) []byte {
	b := make([]byte, (len(have)+7)/8)
	for i, h := range have {
		if h {
			b[i/8] |= 0x80 >> (uint(i) % 8)
		}
	}
	return b
}

// decodeBitfield valida o comprimento e que os bits sobrantes são zero.
func decodeBitfield(b []byte, n int) ([]bool, error) {
	if len(b) != (n+7)/8 {
		return nil, fmt.Errorf("btnative: bitfield com %d bytes (esperado %d)", len(b), (n+7)/8)
	}
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		out[i] = b[i/8]&(0x80>>(uint(i)%8)) != 0
	}
	if rem := n % 8; rem != 0 && b[len(b)-1]&(0xFF>>uint(rem)) != 0 {
		return nil, errors.New("btnative: bitfield com bits sobrantes")
	}
	return out, nil
}

// peers compactos: 4 bytes IP + 2 bytes porta (IPv4) ou 16+2 (IPv6).
func parseCompactPeers(b []byte, v6 bool) []string {
	size := 6
	if v6 {
		size = 18
	}
	var out []string
	for i := 0; i+size <= len(b); i += size {
		ip := net.IP(append([]byte(nil), b[i:i+size-2]...))
		port := binary.BigEndian.Uint16(b[i+size-2 : i+size])
		if port == 0 {
			continue
		}
		out = append(out, net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	}
	return out
}
