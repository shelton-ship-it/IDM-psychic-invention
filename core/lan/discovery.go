package lan

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"time"
)

// DiscoveryPort é a porta UDP de anúncio por defeito.
const DiscoveryPort = 47820

// Announcement é o anúncio (apenas uma PISTA: nada nele é de confiança; o
// pareamento por PIN é que autentica).
type Announcement struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"` // porta TCP do Receiver
}

// Peer é um dispositivo visto na rede.
type Peer struct {
	ID   string
	Name string
	Addr string // ip:porta TCP (IP vem do pacote, não do conteúdo)
}

// Beacon envia o anúncio a cada intervalo até ctx terminar. target por
// defeito é o broadcast da LAN.
func Beacon(ctx context.Context, target string, a Announcement, every time.Duration) error {
	if target == "" {
		target = net.JoinHostPort("255.255.255.255", itoa(DiscoveryPort))
	}
	if every <= 0 {
		every = 2 * time.Second
	}
	a.V, a.Name = 1, cleanName(a.Name)
	raddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return err
	}
	c, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer c.Close()
	b, _ := json.Marshal(a)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		_, _ = c.Write(b)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Browse escuta anúncios em listen (ex.: ":47820") e chama onPeer por cada um
// válido. Pacotes grandes ou malformados são ignorados.
func Browse(ctx context.Context, listen string, onPeer func(Peer)) error {
	pc, err := net.ListenPacket("udp", listen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var a Announcement
		if n >= len(buf) || json.Unmarshal(buf[:n], &a) != nil || a.V != 1 || a.Port < 1 || a.Port > 65535 ||
			a.ID == "" || len(a.ID) > 64 || strings.ContainsAny(a.ID, "\x00\n") {
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		onPeer(Peer{ID: a.ID, Name: cleanName(a.Name), Addr: net.JoinHostPort(udp.IP.String(), itoa(a.Port))})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
