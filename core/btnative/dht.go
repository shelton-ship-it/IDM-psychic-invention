package btnative

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sort"
	"time"

	"streamvault/core/torrent"
)

// Nós de arranque públicos usados quando nada é configurado.
var DefaultDHTBootstrap = []string{
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.utorrent.com:6881",
}

type dhtNode struct {
	id   [20]byte
	addr *net.UDPAddr
	key  string
	dist [20]byte
}

func xor(a, b [20]byte) (d [20]byte) {
	for i := range a {
		d[i] = a[i] ^ b[i]
	}
	return
}

func lessDist(a, b [20]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()
}

// dhtLookup procura peers de infoHash na DHT (cliente de get_peers, BEP 5) e
// chama onPeer por cada peer novo, até ctx terminar ou a pesquisa esgotar.
// Só faz consultas; não anuncia nem responde (não é um nó servidor).
func dhtLookup(ctx context.Context, bootstrap []string, infoHash [20]byte, allowPrivate bool, onPeer func(string)) {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return
	}
	defer pc.Close()
	var self [20]byte
	rand.Read(self[:])

	nodes := map[string]*dhtNode{}
	sent := map[string]bool{} // endereços a quem perguntámos (só aceitamos respostas destes)
	seen := map[string]bool{} // peers já entregues
	add := func(id [20]byte, a *net.UDPAddr) {
		if a == nil || a.Port == 0 || (!allowPrivate && isPrivateIP(a.IP)) {
			return
		}
		k := a.String()
		if _, ok := nodes[k]; ok {
			return
		}
		nodes[k] = &dhtNode{id: id, addr: a, key: k, dist: xor(id, infoHash)}
	}
	for _, b := range bootstrap {
		if a, err := net.ResolveUDPAddr("udp4", b); err == nil {
			add([20]byte{}, a) // id desconhecido: distância máxima
			if n := nodes[a.String()]; n != nil {
				for i := range n.dist {
					n.dist[i] = 0xFF
				}
			}
		}
	}
	const alpha, maxQueries = 8, 300
	queries := 0
	deadline := time.Now().Add(20 * time.Second)
	buf := make([]byte, 4096)
	idle := 0
	for ctx.Err() == nil && time.Now().Before(deadline) && queries < maxQueries && idle < 4 {
		// escolher os alpha nós mais próximos ainda não consultados
		var cand []*dhtNode
		for k, n := range nodes {
			if !sent[k] {
				cand = append(cand, n)
			}
		}
		if len(cand) == 0 {
			idle++
		}
		sort.Slice(cand, func(i, j int) bool { return lessDist(cand[i].dist, cand[j].dist) })
		if len(cand) > alpha {
			cand = cand[:alpha]
		}
		for _, n := range cand {
			tid := make([]byte, 2)
			binary.BigEndian.PutUint16(tid, uint16(queries))
			q, _ := torrent.Encode(map[string]any{"t": tid, "y": "q", "q": "get_peers",
				"a": map[string]any{"id": self[:], "info_hash": infoHash[:]}})
			sent[n.key] = true
			queries++
			pc.WriteTo(q, n.addr)
		}
		// ler respostas durante uma janela curta
		pc.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
		got := false
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				break
			}
			ua, ok := from.(*net.UDPAddr)
			if !ok || !sent[ua.String()] {
				continue // resposta não solicitada: ignorar
			}
			v, err := torrent.Decode(append([]byte(nil), buf[:n]...))
			if err != nil {
				continue
			}
			m, ok := v.(map[string]any)
			if !ok || string(asBytes(m["y"])) != "r" {
				continue
			}
			r, ok := m["r"].(map[string]any)
			if !ok {
				continue
			}
			got = true
			if vals, ok := r["values"].([]any); ok {
				for _, e := range vals {
					for _, p := range parseCompactPeers(asBytes(e), false) {
						if !seen[p] {
							seen[p] = true
							onPeer(p)
						}
					}
				}
			}
			nb := asBytes(r["nodes"])
			for i := 0; i+26 <= len(nb) && i < 26*64; i += 26 {
				var id [20]byte
				copy(id[:], nb[i:i+20])
				ip := net.IP(append([]byte(nil), nb[i+20:i+24]...))
				port := int(binary.BigEndian.Uint16(nb[i+24 : i+26]))
				add(id, &net.UDPAddr{IP: ip, Port: port})
			}
		}
		if got {
			idle = 0
		} else if len(cand) == 0 {
			// sem nós novos e sem respostas: esperar um pouco antes de desistir
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
}

func asBytes(v any) []byte {
	b, _ := v.([]byte)
	return b
}
