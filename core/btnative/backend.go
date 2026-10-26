package btnative

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"streamvault/core/torrent"
	"streamvault/core/torrentengine"
)

// Options do Backend.
type Options struct {
	ListenAddr        string        // defeito ":0" (porta aleatória)
	PeerID            [20]byte      // defeito: "-SV0001-" + 12 bytes aleatórios
	DHT               bool          // procurar peers na DHT (magnets sem trackers)
	DHTBootstrap      []string      // defeito: DefaultDHTBootstrap
	AllowPrivatePeers bool          // permitir peers/trackers em loopback/LAN (defeito: não)
	MaxPeers          int           // por torrent; defeito 50
	UploadSlots       int           // peers desbloqueados para upload; defeito 4
	DialTimeout       time.Duration // defeito 8 s
}

// Backend implementa torrentengine.Backend.
type Backend struct {
	o   Options
	ln  net.Listener
	hc  *http.Client
	mu  sync.Mutex
	ses map[[20]byte]*session
	// fechado
	closed bool
	wg     sync.WaitGroup
}

var _ torrentengine.Backend = (*Backend)(nil)

// New cria o Backend e começa a escutar ligações de peers.
func New(o Options) (*Backend, error) {
	if o.ListenAddr == "" {
		o.ListenAddr = ":0"
	}
	if o.MaxPeers <= 0 {
		o.MaxPeers = 50
	}
	if o.UploadSlots <= 0 {
		o.UploadSlots = 4
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 8 * time.Second
	}
	if len(o.DHTBootstrap) == 0 {
		o.DHTBootstrap = DefaultDHTBootstrap
	}
	if o.PeerID == ([20]byte{}) {
		copy(o.PeerID[:], "-SV0001-")
		rand.Read(o.PeerID[8:])
	}
	ln, err := net.Listen("tcp", o.ListenAddr)
	if err != nil {
		return nil, err
	}
	b := &Backend{o: o, ln: ln, ses: map[[20]byte]*session{},
		hc: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirecionamentos de tracker não seguidos")
		}}}
	b.wg.Add(1)
	go b.acceptLoop()
	return b, nil
}

// Addr é o endereço de escuta (para testes e anúncios).
func (b *Backend) Addr() net.Addr { return b.ln.Addr() }

func (b *Backend) port() int {
	if a, ok := b.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// Close termina todas as sessões e o listener.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	ses := make([]*session, 0, len(b.ses))
	for _, s := range b.ses {
		ses = append(ses, s)
	}
	b.mu.Unlock()
	b.ln.Close()
	for _, s := range ses {
		s.Stop()
	}
	b.wg.Wait()
	return nil
}

func (b *Backend) acceptLoop() {
	defer b.wg.Done()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handleIncoming(c)
		}()
	}
}

func (b *Backend) handleIncoming(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	h, err := readHandshakeHead(c)
	if err != nil {
		c.Close()
		return
	}
	b.mu.Lock()
	s := b.ses[h.infoHash]
	b.mu.Unlock()
	if s == nil || s.ctx.Err() != nil {
		c.Close()
		return
	}
	if _, err := c.Write(encodeHandshake(h.infoHash, b.o.PeerID)); err != nil {
		c.Close()
		return
	}
	if err := readPeerID(c, &h); err != nil || h.peerID == b.o.PeerID {
		c.Close()
		return
	}
	s.addPeer(c, h)
}

// Add cria uma sessão para um magnet ou um .torrent. Não grava nada em disco.
func (b *Backend) Add(ctx context.Context, src torrentengine.Source, destDir string) (torrentengine.Session, error) {
	var ih [20]byte
	var trackers, direct []string
	var meta *torrent.Meta
	switch {
	case len(src.MetaInfo) > 0:
		m, err := torrent.Parse(src.MetaInfo)
		if err != nil {
			return nil, err
		}
		meta = m
		raw, _ := hex.DecodeString(m.InfoHash)
		copy(ih[:], raw)
		trackers = trackersFromTorrent(src.MetaInfo)
	case src.Magnet != "":
		mg, err := parseMagnet(src.Magnet)
		if err != nil {
			return nil, err
		}
		ih, trackers, direct = mg.infoHash, mg.trackers, mg.peers
	default:
		return nil, errors.New("btnative: origem vazia")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("btnative: fechado")
	}
	if _, dup := b.ses[ih]; dup {
		b.mu.Unlock()
		return nil, errors.New("btnative: torrent já ativo")
	}
	s := newSession(b, ih, destDir, trackers, meta, src.MetaInfo)
	b.ses[ih] = s
	b.mu.Unlock()
	for _, p := range direct {
		s.offerAddr(p)
	}
	s.start()
	return s, nil
}

func (b *Backend) remove(s *session) {
	b.mu.Lock()
	if b.ses[s.ih] == s {
		delete(b.ses, s.ih)
	}
	b.mu.Unlock()
}

type magnet struct {
	infoHash [20]byte
	trackers []string
	peers    []string
}

func parseMagnet(raw string) (magnet, error) {
	var m magnet
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "magnet" {
		return m, errors.New("btnative: magnet inválido")
	}
	q := u.Query()
	var xt string
	for _, v := range q["xt"] {
		if strings.HasPrefix(strings.ToLower(v), "urn:btih:") {
			xt = v[len("urn:btih:"):]
			break
		}
	}
	switch len(xt) {
	case 40:
		b, err := hex.DecodeString(xt)
		if err != nil {
			return m, errors.New("btnative: info-hash hex inválido")
		}
		copy(m.infoHash[:], b)
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(xt))
		if err != nil || len(b) != 20 {
			return m, errors.New("btnative: info-hash base32 inválido")
		}
		copy(m.infoHash[:], b)
	default:
		return m, errors.New("btnative: magnet sem info-hash BitTorrent v1")
	}
	for _, t := range q["tr"] {
		if len(m.trackers) < 20 && len(t) <= 512 {
			m.trackers = append(m.trackers, t)
		}
	}
	for _, p := range q["x.pe"] {
		if _, _, err := net.SplitHostPort(p); err == nil && len(m.peers) < 20 {
			m.peers = append(m.peers, p)
		}
	}
	return m, nil
}

func trackersFromTorrent(raw []byte) []string {
	v, err := torrent.Decode(raw)
	if err != nil {
		return nil
	}
	root, _ := v.(map[string]any)
	var out []string
	add := func(x any) {
		if b, ok := x.([]byte); ok && len(out) < 20 && len(b) <= 512 {
			out = append(out, string(b))
		}
	}
	add(root["announce"])
	if al, ok := root["announce-list"].([]any); ok {
		for _, tier := range al {
			if l, ok := tier.([]any); ok {
				for _, t := range l {
					add(t)
				}
			}
		}
	}
	return out
}

func fmtErr(format string, a ...any) error { return fmt.Errorf("btnative: "+format, a...) }
