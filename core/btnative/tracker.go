package btnative

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"streamvault/core/security"
	"streamvault/core/torrent"
)

// announceReq são os dados de um anúncio.
type announceReq struct {
	infoHash   [20]byte
	peerID     [20]byte
	port       int
	downloaded int64
	left       int64
	event      string // "started" | "stopped" | "completed" | ""
}

type announceResp struct {
	interval time.Duration
	peers    []string
}

// checkTrackerURL aplica a política de rede: os trackers vêm de magnets de
// terceiros, portanto um tracker em loopback/rede privada só é contactado se
// as redes privadas forem permitidas (SSRF).
func checkTrackerURL(raw string, allowPrivate bool) error {
	opts := security.DefaultOptions()
	opts.AllowPrivateNetworks = allowPrivate
	for _, f := range security.CheckURL(raw, opts) {
		if f.Severity == "block" {
			return fmt.Errorf("tracker bloqueado: %s", f.Message)
		}
	}
	return nil
}

func announce(ctx context.Context, tracker string, r announceReq, allowPrivate bool, hc *http.Client) (announceResp, error) {
	u, err := url.Parse(tracker)
	if err != nil {
		return announceResp{}, err
	}
	switch u.Scheme {
	case "http", "https":
		if err := checkTrackerURL(tracker, allowPrivate); err != nil {
			return announceResp{}, err
		}
		return announceHTTP(ctx, u, r, hc)
	case "udp":
		// udp:// não passa por CheckURL (esquema); validar o host manualmente
		if err := checkTrackerURL("http://"+u.Host, allowPrivate); err != nil {
			return announceResp{}, err
		}
		return announceUDP(ctx, u.Host, r)
	}
	return announceResp{}, fmt.Errorf("esquema de tracker não suportado: %q", u.Scheme)
}

func announceHTTP(ctx context.Context, u *url.URL, r announceReq, hc *http.Client) (announceResp, error) {
	q := "info_hash=" + url.QueryEscape(string(r.infoHash[:])) +
		"&peer_id=" + url.QueryEscape(string(r.peerID[:])) +
		fmt.Sprintf("&port=%d&uploaded=0&downloaded=%d&left=%d&compact=1&numwant=50", r.port, r.downloaded, r.left)
	if r.event != "" {
		q += "&event=" + r.event
	}
	full := u.String()
	if strings.Contains(full, "?") {
		full += "&" + q
	} else {
		full += "?" + q
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return announceResp{}, err
	}
	req.Header.Set("User-Agent", "StreamVault/1")
	resp, err := hc.Do(req)
	if err != nil {
		return announceResp{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return announceResp{}, fmt.Errorf("tracker respondeu HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return announceResp{}, err
	}
	v, err := torrent.Decode(body)
	if err != nil {
		return announceResp{}, fmt.Errorf("resposta do tracker inválida: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return announceResp{}, errors.New("resposta do tracker inválida")
	}
	if fr, ok := m["failure reason"].([]byte); ok {
		return announceResp{}, fmt.Errorf("tracker recusou: %s", cleanASCII(string(fr)))
	}
	out := announceResp{interval: 30 * time.Minute}
	if iv, ok := m["interval"].(int64); ok && iv > 0 {
		out.interval = time.Duration(iv) * time.Second
	}
	switch p := m["peers"].(type) {
	case []byte:
		out.peers = parseCompactPeers(p, false)
	case []any: // formato não compacto
		for _, e := range p {
			d, ok := e.(map[string]any)
			if !ok {
				continue
			}
			ip, _ := d["ip"].([]byte)
			port, _ := d["port"].(int64)
			if len(ip) > 0 && port > 0 && port < 65536 {
				out.peers = append(out.peers, net.JoinHostPort(string(ip), fmt.Sprint(port)))
			}
		}
	}
	if p6, ok := m["peers6"].([]byte); ok {
		out.peers = append(out.peers, parseCompactPeers(p6, true)...)
	}
	return out, nil
}

// cleanASCII (nome histórico) remove caracteres de controlo e limita o tamanho
// de texto vindo de um tracker remoto antes de o mostrar ao utilizador.
func cleanASCII(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if unicode.IsPrint(r) && len(b) < 120 {
			b = append(b, r)
		}
	}
	return string(b)
}

// announceUDP implementa o protocolo de trackers UDP (BEP 15).
func announceUDP(ctx context.Context, hostport string, r announceReq) (announceResp, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", hostport)
	if err != nil {
		return announceResp{}, err
	}
	defer c.Close()

	roundTrip := func(req []byte, minResp int) ([]byte, error) {
		buf := make([]byte, 2048)
		for try := 0; try < 3; try++ {
			c.SetDeadline(time.Now().Add(time.Duration(3+try*2) * time.Second))
			if _, err := c.Write(req); err != nil {
				return nil, err
			}
			n, err := c.Read(buf)
			if err == nil && n >= minResp {
				return buf[:n], nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("tracker UDP sem resposta")
	}

	tx := make([]byte, 4)
	rand.Read(tx)
	conn := make([]byte, 16)
	binary.BigEndian.PutUint64(conn[0:], 0x41727101980) // protocol_id
	binary.BigEndian.PutUint32(conn[8:], 0)             // action connect
	copy(conn[12:], tx)
	resp, err := roundTrip(conn, 16)
	if err != nil {
		return announceResp{}, err
	}
	if binary.BigEndian.Uint32(resp[0:]) != 0 || string(resp[4:8]) != string(tx) {
		return announceResp{}, errors.New("resposta connect inválida")
	}
	connID := resp[8:16]

	ev := uint32(0)
	switch r.event {
	case "completed":
		ev = 1
	case "started":
		ev = 2
	case "stopped":
		ev = 3
	}
	rand.Read(tx)
	a := make([]byte, 98)
	copy(a[0:], connID)
	binary.BigEndian.PutUint32(a[8:], 1) // announce
	copy(a[12:], tx)
	copy(a[16:], r.infoHash[:])
	copy(a[36:], r.peerID[:])
	binary.BigEndian.PutUint64(a[56:], uint64(r.downloaded))
	binary.BigEndian.PutUint64(a[64:], uint64(r.left))
	binary.BigEndian.PutUint64(a[72:], 0)
	binary.BigEndian.PutUint32(a[80:], ev)
	// ip=0 (a[84:88]), key aleatória
	rand.Read(a[88:92])
	binary.BigEndian.PutUint32(a[92:], 0xFFFFFFFF) // num_want = -1
	binary.BigEndian.PutUint16(a[96:], uint16(r.port))
	resp, err = roundTrip(a, 20)
	if err != nil {
		return announceResp{}, err
	}
	act := binary.BigEndian.Uint32(resp[0:])
	if act == 3 {
		return announceResp{}, fmt.Errorf("tracker recusou: %s", cleanASCII(string(resp[8:])))
	}
	if act != 1 || string(resp[4:8]) != string(tx) {
		return announceResp{}, errors.New("resposta announce inválida")
	}
	out := announceResp{interval: time.Duration(binary.BigEndian.Uint32(resp[8:])) * time.Second}
	if out.interval <= 0 {
		out.interval = 30 * time.Minute
	}
	out.peers = parseCompactPeers(resp[20:], false)
	return out, nil
}
