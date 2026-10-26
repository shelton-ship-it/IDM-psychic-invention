package btnative

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"streamvault/core/torrent"
	"streamvault/core/torrentengine"
)

var _ torrentengine.Session = (*session)(nil)

const (
	pipeline      = 8 // blocos pedidos em simultâneo por peer
	stallTimeout  = 60 * time.Second
	maxBadPieces  = 3 // peças inválidas tolerados por peer antes de o expulsar
	addrQueueSize = 512
)

// assign é uma peça atribuída a UM peer enquanto os blocos chegam.
type assign struct {
	idx         int
	buf         []byte
	got         []bool
	nblocks     int
	have        int
	next        int
	outstanding int
	peer        *peer
	last        time.Time
	verifying   bool
}

type session struct {
	b        *Backend
	ih       [20]byte
	destDir  string
	trackers []string
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	meta      *torrent.Meta
	rawMeta   []byte
	metaErr   error
	metaReady chan struct{}
	mdSize    int
	mdPieces  map[int][]byte

	started    bool
	selected   map[int]bool // nil = todos
	needed     []bool
	have       []bool
	servable   []bool // peças que podemos servir (todos os ficheiros gravados)
	selBytes   []int64
	doneBytes  int64
	totalBytes int64
	assigned   map[int]*assign
	files      map[int]*os.File
	complete   bool
	done       chan struct{}
	fatal      error

	peers    map[*peer]bool
	tried    map[string]time.Time
	addrCh   chan string
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newSession(b *Backend, ih [20]byte, destDir string, trackers []string, meta *torrent.Meta, raw []byte) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{b: b, ih: ih, destDir: destDir, trackers: trackers, ctx: ctx, cancel: cancel,
		metaReady: make(chan struct{}), done: make(chan struct{}), assigned: map[int]*assign{},
		files: map[int]*os.File{}, peers: map[*peer]bool{}, tried: map[string]time.Time{},
		addrCh: make(chan string, addrQueueSize), mdPieces: map[int][]byte{}}
	if meta != nil {
		s.meta, s.rawMeta = meta, raw
		close(s.metaReady)
	}
	return s
}

func (s *session) start() {
	s.wg.Add(1)
	go s.connectLoop()
	for _, tr := range s.trackers {
		tr := tr
		s.wg.Add(1)
		go s.trackerLoop(tr)
	}
	if s.b.o.DHT {
		s.wg.Add(1)
		go s.dhtLoop()
	}
}

// ---- torrentengine.Session ----

func (s *session) Meta(ctx context.Context) ([]byte, error) {
	select {
	case <-s.metaReady:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.metaErr != nil {
			return nil, s.metaErr
		}
		return s.rawMeta, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, errors.New("btnative: sessão parada")
	}
}

func (s *session) Done() <-chan struct{} { return s.done }

// VerifiesPieces: este backend confere o SHA-1 de cada peça antes de a gravar
// (ver onPiece), por isso uma seleção sem peças inteiras continua provada.
func (s *session) VerifiesPieces() bool { return true }

func (s *session) Progress() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneBytes, s.totalBytes
}

// Download escolhe os ficheiros, retoma o que já está bom em disco e começa a pedir peças.
func (s *session) Download(selected map[int]bool) error {
	s.mu.Lock()
	if s.meta == nil {
		s.mu.Unlock()
		return errors.New("btnative: metadados ainda indisponíveis")
	}
	if s.started {
		s.mu.Unlock()
		return nil
	}
	m := s.meta
	n := len(m.Pieces)
	s.selected = selected
	s.needed, s.have, s.servable = make([]bool, n), make([]bool, n), make([]bool, n)
	s.selBytes = make([]int64, n)
	isSel := func(i int) bool { return selected == nil || selected[i] }
	for _, f := range m.Files {
		if isSel(f.Index) {
			s.totalBytes += f.Length
		}
	}
	for i := 0; i < n; i++ {
		for _, sp := range m.PieceSpans(i) {
			if isSel(sp.File.Index) {
				s.needed[i] = true
				s.selBytes[i] += sp.To - sp.From
			}
		}
	}
	// abrir/criar os ficheiros selecionados (caminhos já validados em Parse)
	for _, f := range m.Files {
		if !isSel(f.Index) {
			continue
		}
		p, err := m.DiskPath(s.destDir, f)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			s.mu.Unlock()
			return err
		}
		fh, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if st, err := fh.Stat(); err == nil && st.Size() < f.Length {
			_ = fh.Truncate(f.Length)
		}
		s.files[f.Index] = fh
	}
	// retoma: peças já corretas em disco
	for i := 0; i < n; i++ {
		if !s.needed[i] {
			continue
		}
		if ok, err := torrent.VerifyPiece(s.destDir, m, i); err == nil && ok {
			s.have[i] = true
			s.doneBytes += s.selBytes[i]
			s.servable[i] = s.fullySelectedLocked(i)
		}
	}
	s.started = true
	s.checkCompleteLocked()
	peers := s.peerListLocked()
	s.mu.Unlock()
	for _, p := range peers {
		p.afterStart()
	}
	return nil
}

// Stop interrompe a rede e mantém os dados em disco.
func (s *session) Stop() error {
	s.stopOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		peers := s.peerListLocked()
		s.mu.Unlock()
		for _, p := range peers {
			p.conn.Close()
		}
		close2 := make(chan struct{})
		go func() { s.wg.Wait(); close(close2) }()
		select {
		case <-close2:
		case <-time.After(6 * time.Second): // anúncio "stopped" é best-effort
		}
		s.b.remove(s)
		s.mu.Lock()
		for _, f := range s.files {
			f.Close()
		}
		s.mu.Unlock()
	})
	return nil
}

// ---- estado ----

func (s *session) peerListLocked() []*peer {
	out := make([]*peer, 0, len(s.peers))
	for p := range s.peers {
		out = append(out, p)
	}
	return out
}

func (s *session) fullySelectedLocked(i int) bool {
	if s.selected == nil {
		return true
	}
	for _, sp := range s.meta.PieceSpans(i) {
		if !s.selected[sp.File.Index] {
			return false
		}
	}
	return true
}

func (s *session) checkCompleteLocked() {
	if s.complete || !s.started {
		return
	}
	for i, need := range s.needed {
		if need && !s.have[i] {
			return
		}
	}
	s.complete = true
	close(s.done)
}

func (s *session) failLocked(err error) {
	if s.fatal == nil {
		s.fatal = err
	}
	if !s.complete && s.started {
		s.complete = true
		close(s.done) // o motor verifica o disco e reporta a falha concreta
	}
}

func (s *session) left() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return 1
	}
	if l := s.totalBytes - s.doneBytes; l > 0 {
		return l
	}
	return 0
}

// ---- descoberta de peers ----

func (s *session) offerAddr(a string) {
	select {
	case s.addrCh <- a:
	default:
	}
}

func (s *session) trackerLoop(tr string) {
	defer s.wg.Done()
	event := "started"
	backoff := 30 * time.Second
	var doneCh <-chan struct{} = s.done
	sent := false
	for {
		var wait time.Duration
		actx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
		s.mu.Lock()
		dl := s.doneBytes
		s.mu.Unlock()
		resp, err := announce(actx, tr, announceReq{infoHash: s.ih, peerID: s.b.o.PeerID, port: s.b.port(),
			downloaded: dl, left: s.left(), event: event}, s.b.o.AllowPrivatePeers, s.b.hc)
		cancel()
		if err == nil {
			for _, p := range resp.peers {
				s.offerAddr(p)
			}
			event = ""
			wait = resp.interval
			if wait < 60*time.Second {
				wait = 60 * time.Second
			}
			backoff = 30 * time.Second
		} else {
			wait = backoff
			if backoff < 10*time.Minute {
				backoff *= 2
			}
		}
		for waiting := true; waiting; {
			select {
			case <-s.ctx.Done():
				s.announceStopped(tr)
				return
			case <-doneCh: // concluído: avisar uma vez
				doneCh = nil
				if !sent {
					sent = true
					cctx, c2 := context.WithTimeout(s.ctx, 10*time.Second)
					announce(cctx, tr, announceReq{infoHash: s.ih, peerID: s.b.o.PeerID, port: s.b.port(),
						downloaded: s.doneSnapshot(), left: 0, event: "completed"}, s.b.o.AllowPrivatePeers, s.b.hc)
					c2()
				}
			case <-time.After(wait):
				waiting = false
			}
		}
	}
}

func (s *session) doneSnapshot() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneBytes
}

func (s *session) announceStopped(tr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	announce(ctx, tr, announceReq{infoHash: s.ih, peerID: s.b.o.PeerID, port: s.b.port(),
		downloaded: s.doneSnapshot(), left: s.left(), event: "stopped"}, s.b.o.AllowPrivatePeers, s.b.hc)
}

func (s *session) dhtLoop() {
	defer s.wg.Done()
	for {
		dhtLookup(s.ctx, s.b.o.DHTBootstrap, s.ih, s.b.o.AllowPrivatePeers, s.offerAddr)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
	}
}

func (s *session) connectLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case a := <-s.addrCh:
			host, _, err := net.SplitHostPort(a)
			ip := net.ParseIP(host)
			if err != nil || ip == nil { // só IPs literais: não resolvemos nomes de terceiros
				continue
			}
			if !s.b.o.AllowPrivatePeers && isPrivateIP(ip) {
				continue
			}
			s.mu.Lock()
			last, seen := s.tried[a]
			full := len(s.peers) >= s.b.o.MaxPeers
			if seen && time.Since(last) < 60*time.Second || full {
				s.mu.Unlock()
				continue
			}
			s.tried[a] = time.Now()
			if len(s.tried) > 4096 {
				s.tried = map[string]time.Time{a: time.Now()}
			}
			s.mu.Unlock()
			s.wg.Add(1)
			go func(addr string) {
				defer s.wg.Done()
				s.dial(addr)
			}(a)
		}
	}
}

func (s *session) dial(addr string) {
	d := net.Dialer{Timeout: s.b.o.DialTimeout}
	c, err := d.DialContext(s.ctx, "tcp", addr)
	if err != nil {
		return
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(encodeHandshake(s.ih, s.b.o.PeerID)); err != nil {
		c.Close()
		return
	}
	h, err := readHandshakeHead(c)
	if err != nil || h.infoHash != s.ih || readPeerID(c, &h) != nil || h.peerID == s.b.o.PeerID {
		c.Close()
		return
	}
	s.addPeer(c, h)
}

func (s *session) addPeer(c net.Conn, h handshake) {
	p := &peer{s: s, conn: c, remote: h, choked: true, addr: c.RemoteAddr().String()}
	s.mu.Lock()
	if s.ctx.Err() != nil || len(s.peers) >= s.b.o.MaxPeers {
		s.mu.Unlock()
		c.Close()
		return
	}
	s.peers[p] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		p.run()
	}()
}

// ---- metadados (BEP 9) ----

// metaPieceDone integra uma peça de metadados; quando completa, verifica o
// SHA-1 contra o info-hash do magnet. Devolve false se o peer enviou lixo.
func (s *session) metaPieceDone(totalSize, idx int, data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta != nil || s.metaErr != nil {
		return true
	}
	if s.mdSize == 0 {
		return true // não pedimos nada ainda
	}
	if totalSize != s.mdSize {
		return false
	}
	np := (s.mdSize + metadataPiece - 1) / metadataPiece
	if idx < 0 || idx >= np {
		return false
	}
	want := metadataPiece
	if idx == np-1 {
		want = s.mdSize - metadataPiece*(np-1)
	}
	if len(data) != want {
		return false
	}
	s.mdPieces[idx] = append([]byte(nil), data...)
	if len(s.mdPieces) < np {
		return true
	}
	raw := make([]byte, 0, s.mdSize)
	for i := 0; i < np; i++ {
		raw = append(raw, s.mdPieces[i]...)
	}
	sum := sha1.Sum(raw)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(s.ih[:]) {
		// metadados adulterados: recomeçar a recolha com outros peers
		s.mdPieces, s.mdSize = map[int][]byte{}, 0
		return false
	}
	full := append(append([]byte("d4:info"), raw...), 'e')
	m, err := torrent.Parse(full)
	if err != nil {
		s.metaErr = err
		close(s.metaReady)
		return true
	}
	s.meta, s.rawMeta = m, full
	close(s.metaReady)
	for p := range s.peers {
		p.applyPendingLocked()
	}
	return true
}

func (s *session) wantMetaSize(size int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta != nil || size <= 0 || size > torrent.MaxMetaSize {
		return false
	}
	if s.mdSize == 0 {
		s.mdSize, s.mdPieces = size, map[int][]byte{}
	}
	return s.mdSize == size
}

var rng = rand.New(rand.NewSource(time.Now().UnixNano()))
var rngMu sync.Mutex

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	rngMu.Lock()
	defer rngMu.Unlock()
	return rng.Intn(n)
}
