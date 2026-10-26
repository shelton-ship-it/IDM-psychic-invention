package btnative

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"streamvault/core/torrent"
)

// ---- protocolo: bytes literais da especificação ----

func TestHandshakeBytesMatchSpec(t *testing.T) {
	var ih, pid [20]byte
	for i := range ih {
		ih[i], pid[i] = byte(i), byte(100+i)
	}
	got := encodeHandshake(ih, pid)
	want := []byte("\x13BitTorrent protocol")
	want = append(want, 0, 0, 0, 0, 0, 0x10, 0, 0) // bit de extensões (BEP 10)
	want = append(want, ih[:]...)
	want = append(want, pid[:]...)
	if !bytes.Equal(got, want) || len(got) != 68 {
		t.Fatalf("handshake diverge da especificação:\n got %x\nwant %x", got, want)
	}
	h, err := readHandshakeHead(bytes.NewReader(got[:48]))
	if err != nil || h.infoHash != ih || !h.supportsExtensions() {
		t.Fatalf("leitura: %+v %v", h, err)
	}
	if _, err := readHandshakeHead(bytes.NewReader(append([]byte{0x12}, got[1:48]...))); err == nil {
		t.Error("pstrlen errado deve falhar")
	}
	if _, err := readHandshakeHead(bytes.NewReader(append([]byte("\x13BitTorrenX protocol"), got[20:48]...))); err == nil {
		t.Error("pstr errado deve falhar")
	}
}

func TestMessageFramingAndLimits(t *testing.T) {
	var b bytes.Buffer
	writeMsg(&b, msgHave, put32(7))
	if !bytes.Equal(b.Bytes(), []byte{0, 0, 0, 5, 4, 0, 0, 0, 7}) {
		t.Fatalf("have mal enquadrado: %x", b.Bytes())
	}
	id, p, err := readMsg(&b)
	if err != nil || id != msgHave || u32(p) != 7 {
		t.Fatalf("%d %x %v", id, p, err)
	}
	if id, _, _ := readMsg(bytes.NewReader([]byte{0, 0, 0, 0})); id != 255 {
		t.Error("keepalive deve devolver 255")
	}
	// comprimento absurdo: recusado sem alocar
	if _, _, err := readMsg(bytes.NewReader([]byte{0x7F, 0xFF, 0xFF, 0xFF})); err == nil {
		t.Error("mensagem gigante deve ser recusada")
	}
}

func TestBitfieldEncodingMSBFirstAndStrictDecoding(t *testing.T) {
	have := []bool{true, false, true, false, false, false, false, false, true, true}
	b := encodeBitfield(have)
	if !bytes.Equal(b, []byte{0b10100000, 0b11000000}) {
		t.Fatalf("bitfield: %08b", b)
	}
	got, err := decodeBitfield(b, len(have))
	if err != nil {
		t.Fatal(err)
	}
	for i := range have {
		if got[i] != have[i] {
			t.Fatalf("bit %d", i)
		}
	}
	if _, err := decodeBitfield([]byte{0b10100000}, 10); err == nil {
		t.Error("comprimento errado deve falhar")
	}
	if _, err := decodeBitfield([]byte{0b10100000, 0b11000001}, 10); err == nil {
		t.Error("bits sobrantes diferentes de zero devem falhar")
	}
}

func TestCompactPeers(t *testing.T) {
	b := []byte{127, 0, 0, 1, 0x1A, 0xE1, 10, 0, 0, 2, 0, 0 /*porta 0 ignorada*/}
	got := parseCompactPeers(b, false)
	if len(got) != 1 || got[0] != "127.0.0.1:6881" {
		t.Fatalf("%v", got)
	}
}

func TestParseMagnet(t *testing.T) {
	hex40 := strings.Repeat("ab", 20)
	m, err := parseMagnet("magnet:?xt=urn:btih:" + hex40 + "&dn=x&tr=http%3A%2F%2Ft.example%2Fannounce&tr=udp%3A%2F%2Ft2.example%3A80&x.pe=1.2.3.4:5555&x.pe=lixo")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.trackers) != 2 || m.trackers[0] != "http://t.example/announce" || len(m.peers) != 1 || m.infoHash[0] != 0xab {
		t.Fatalf("%+v", m)
	}
	for _, bad := range []string{"http://x", "magnet:?dn=x", "magnet:?xt=urn:btih:zz", "magnet:?xt=urn:btmh:1220abcd"} {
		if _, err := parseMagnet(bad); err == nil {
			t.Errorf("%q deveria falhar", bad)
		}
	}
}

// ---- trackers ----

func TestHTTPTrackerEncodesRawInfoHashAndParsesCompact(t *testing.T) {
	var ih, pid [20]byte
	for i := range ih {
		ih[i] = byte(0xF0 + i%16) // bytes altos: exigem percent-encoding correto
		pid[i] = byte('a' + i)
	}
	var mu sync.Mutex
	var seen map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		seen = map[string]string{"ih": q.Get("info_hash"), "pid": q.Get("peer_id"), "port": q.Get("port"),
			"left": q.Get("left"), "compact": q.Get("compact"), "event": q.Get("event"), "key": q.Get("passkey")}
		mu.Unlock()
		peers := string([]byte{10, 1, 2, 3, 0x1A, 0xE1, 127, 0, 0, 1, 0, 80})
		body, _ := torrent.Encode(map[string]any{"interval": int64(120), "peers": peers})
		w.Write(body)
	}))
	defer srv.Close()
	resp, err := announce(context.Background(), srv.URL+"/announce?passkey=XYZ", announceReq{infoHash: ih, peerID: pid,
		port: 6881, left: 1234, event: "started"}, true, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["ih"] != string(ih[:]) || seen["pid"] != string(pid[:]) || seen["port"] != "6881" ||
		seen["left"] != "1234" || seen["compact"] != "1" || seen["event"] != "started" || seen["key"] != "XYZ" {
		t.Fatalf("o tracker recebeu parâmetros errados: %q", seen)
	}
	if resp.interval != 120*time.Second || len(resp.peers) != 2 || resp.peers[0] != "10.1.2.3:6881" || resp.peers[1] != "127.0.0.1:80" {
		t.Fatalf("resposta: %+v", resp)
	}
}

func TestHTTPTrackerFailureNonCompactAndSSRF(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := torrent.Encode(map[string]any{"failure reason": "torrent não registado\x00"})
		w.Write(b)
	}))
	defer fail.Close()
	if _, err := announce(context.Background(), fail.URL, announceReq{}, true, http.DefaultClient); err == nil || !strings.Contains(err.Error(), "torrent não registado") {
		t.Fatalf("failure reason deve propagar: %v", err)
	}
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := torrent.Encode(map[string]any{"interval": int64(60), "peers": []any{
			map[string]any{"ip": "9.9.9.9", "port": int64(1234)}, map[string]any{"ip": "x", "port": int64(0)}}})
		w.Write(b)
	}))
	defer list.Close()
	r, err := announce(context.Background(), list.URL, announceReq{}, true, http.DefaultClient)
	if err != nil || len(r.peers) != 1 || r.peers[0] != "9.9.9.9:1234" {
		t.Fatalf("lista não compacta: %+v %v", r, err)
	}
	// um magnet de terceiros não pode fazer-nos contactar serviços locais
	if _, err := announce(context.Background(), list.URL, announceReq{}, false, http.DefaultClient); err == nil {
		t.Fatal("tracker em loopback deve ser bloqueado quando peers privados não são permitidos")
	}
	if _, err := announce(context.Background(), "udp://127.0.0.1:9/announce", announceReq{}, false, http.DefaultClient); err == nil {
		t.Fatal("tracker UDP em loopback deve ser bloqueado")
	}
	if _, err := announce(context.Background(), "ftp://x/y", announceReq{}, true, http.DefaultClient); err == nil {
		t.Fatal("esquema desconhecido deve falhar")
	}
}

// Servidor UDP independente, escrito com offsets literais de BEP 15.
func TestUDPTrackerFollowsBEP15(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var ih, pid [20]byte
	copy(ih[:], "01234567890123456789")
	copy(pid[:], "-SV0001-abcdefghijkl")
	errc := make(chan string, 1)
	go func() {
		buf := make([]byte, 2048)
		connID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		for step := 0; step < 2; step++ {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			p := buf[:n]
			if step == 0 {
				if n != 16 || binary.BigEndian.Uint64(p[0:]) != 0x41727101980 || binary.BigEndian.Uint32(p[8:]) != 0 {
					errc <- "connect request inválido"
					return
				}
				resp := append([]byte{0, 0, 0, 0}, p[12:16]...)
				pc.WriteTo(append(resp, connID...), from)
			} else {
				switch {
				case n != 98:
					errc <- "announce com tamanho errado"
				case !bytes.Equal(p[0:8], connID):
					errc <- "connection_id errado"
				case binary.BigEndian.Uint32(p[8:]) != 1:
					errc <- "action != announce"
				case !bytes.Equal(p[16:36], ih[:]) || !bytes.Equal(p[36:56], pid[:]):
					errc <- "info_hash/peer_id nos offsets errados"
				case binary.BigEndian.Uint64(p[64:]) != 777:
					errc <- "left no offset errado"
				case binary.BigEndian.Uint32(p[80:]) != 2:
					errc <- "event started deve ser 2"
				case binary.BigEndian.Uint16(p[96:]) != 6881:
					errc <- "porta no offset errado"
				}
				resp := append([]byte{0, 0, 0, 1}, p[12:16]...)
				resp = append(resp, 0, 0, 0, 90, 0, 0, 0, 1, 0, 0, 0, 2) // interval, leechers, seeders
				resp = append(resp, 8, 8, 4, 4, 0x1F, 0x90)              // 8.8.4.4:8080
				pc.WriteTo(resp, from)
			}
		}
		errc <- ""
	}()
	r, err := announce(context.Background(), "udp://"+pc.LocalAddr().String()+"/announce",
		announceReq{infoHash: ih, peerID: pid, port: 6881, left: 777, event: "started"}, true, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if msg := <-errc; msg != "" {
		t.Fatal(msg)
	}
	if r.interval != 90*time.Second || len(r.peers) != 1 || r.peers[0] != "8.8.4.4:8080" {
		t.Fatalf("%+v", r)
	}
}

// ---- DHT ----

func TestDHTLookupFollowsNodesAndIgnoresUnsolicited(t *testing.T) {
	var ih [20]byte
	copy(ih[:], "ABCDEFGHIJKLMNOPQRST")
	mk := func() net.PacketConn {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return pc
	}
	nodeB, nodeA, rogue := mk(), mk(), mk()
	defer nodeA.Close()
	defer nodeB.Close()
	defer rogue.Close()
	reply := func(pc net.PacketConn, r map[string]any) {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			v, err := torrent.Decode(append([]byte(nil), buf[:n]...))
			if err != nil {
				continue
			}
			q := v.(map[string]any)
			if string(q["q"].([]byte)) != "get_peers" {
				continue
			}
			a := q["a"].(map[string]any)
			if !bytes.Equal(a["info_hash"].([]byte), ih[:]) {
				continue
			}
			rr := map[string]any{"id": make([]byte, 20)}
			for k, v := range r {
				rr[k] = v
			}
			b, _ := torrent.Encode(map[string]any{"t": q["t"], "y": "r", "r": rr})
			pc.WriteTo(b, from)
		}
	}
	// B conhece o peer; A só conhece B (e devolve-o em "nodes")
	bAddr := nodeB.LocalAddr().(*net.UDPAddr)
	node := make([]byte, 26)
	copy(node[20:], bAddr.IP.To4())
	binary.BigEndian.PutUint16(node[24:], uint16(bAddr.Port))
	go reply(nodeB, map[string]any{"values": []any{string([]byte{5, 6, 7, 8, 0x13, 0x88})}})
	go reply(nodeA, map[string]any{"nodes": string(node)})
	// um terceiro envia "valores" que ninguém lhe pediu: tem de ser ignorado
	go func() {
		for i := 0; i < 20; i++ {
			b, _ := torrent.Encode(map[string]any{"t": "xx", "y": "r", "r": map[string]any{"id": make([]byte, 20),
				"values": []any{string([]byte{6, 6, 6, 6, 0x13, 0x88})}}})
			rogue.WriteTo(b, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
			time.Sleep(10 * time.Millisecond)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	got := make(chan string, 8)
	go dhtLookup(ctx, []string{nodeA.LocalAddr().String()}, ih, true, func(p string) { got <- p })
	select {
	case p := <-got:
		if p != "5.6.7.8:5000" {
			t.Fatalf("peer inesperado: %s", p)
		}
	case <-ctx.Done():
		t.Fatal("DHT não seguiu os nós até ao peer")
	}
	select {
	case p := <-got:
		if p == "6.6.6.6:5000" {
			t.Fatal("resposta não solicitada foi aceite")
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDHTIgnoresPrivateNodesUnlessAllowed(t *testing.T) {
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer pc.Close()
	hit := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 512)
		if _, _, err := pc.ReadFrom(buf); err == nil {
			hit <- struct{}{}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	dhtLookup(ctx, []string{pc.LocalAddr().String()}, [20]byte{1}, false, func(string) {})
	select {
	case <-hit:
		t.Fatal("nó de arranque privado não deveria ser contactado com peers privados desligados")
	default:
	}
}
