package btnative

import (
	"crypto/sha1"
	"net"
	"sync"
	"time"

	"streamvault/core/torrent"
)

type peer struct {
	s      *session
	conn   net.Conn
	addr   string
	remote handshake
	wmu    sync.Mutex
	done   chan struct{}

	// protegidos por s.mu
	choked         bool // o peer faz-nos choke
	amInterested   bool
	theyInterested bool
	weUnchoke      bool
	bfSent         bool
	has            []bool
	pendBF         []byte
	pendHave       []uint32
	remoteMD       int
	cur            *assign
	bad            int
	lastRecv       time.Time
	closed         bool // desligado: nunca mais recebe atribuições
}

func (p *peer) send(id byte, payload ...[]byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return writeMsg(p.conn, id, payload...)
}

func (p *peer) run() {
	s := p.s
	p.done = make(chan struct{})
	defer p.cleanup()
	s.mu.Lock()
	p.lastRecv = time.Now()
	s.mu.Unlock()
	go p.watchdog()
	if p.remote.supportsExtensions() {
		p.sendExtHandshake()
	}
	p.afterStart()
	for {
		p.conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		id, payload, err := readMsg(p.conn)
		if err != nil {
			return
		}
		s.mu.Lock()
		p.lastRecv = time.Now()
		s.mu.Unlock()
		if !p.handle(id, payload) {
			return
		}
	}
}

func (p *peer) cleanup() {
	s := p.s
	s.mu.Lock()
	p.closed = true // um fill() atrasado (ex.: via kick) já não pode atribuir peças a este peer
	delete(s.peers, p)
	released := false
	if p.cur != nil {
		delete(s.assigned, p.cur.idx)
		p.cur = nil
		released = true
	}
	s.mu.Unlock()
	p.conn.Close()
	close(p.done)
	if released {
		s.kick()
	}
}

// kick pede a todos os peers para tentarem pedir peças (após libertar uma atribuição).
func (s *session) kick() {
	s.mu.Lock()
	peers := s.peerListLocked()
	s.mu.Unlock()
	for _, p := range peers {
		go p.fill()
	}
}

func (p *peer) watchdog() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	lastKA := time.Now()
	for {
		select {
		case <-p.done:
			return
		case <-p.s.ctx.Done():
			p.conn.Close()
			return
		case <-t.C:
			p.s.mu.Lock()
			stalled := p.cur != nil && time.Since(p.cur.last) > stallTimeout
			p.s.mu.Unlock()
			if stalled {
				p.conn.Close()
				return
			}
			if time.Since(lastKA) > 90*time.Second {
				lastKA = time.Now()
				p.wmu.Lock()
				p.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				writeKeepAlive(p.conn)
				p.wmu.Unlock()
			}
		}
	}
}

// handle devolve false para desligar o peer.
func (p *peer) handle(id byte, payload []byte) bool {
	s := p.s
	switch id {
	case 255: // keepalive
	case msgChoke:
		s.mu.Lock()
		p.choked = true
		released := false
		if p.cur != nil {
			delete(s.assigned, p.cur.idx)
			p.cur = nil
			released = true
		}
		s.mu.Unlock()
		if released {
			s.kick()
		}
	case msgUnchoke:
		s.mu.Lock()
		p.choked = false
		s.mu.Unlock()
		p.fill()
	case msgInterested:
		s.mu.Lock()
		p.theyInterested = true
		s.mu.Unlock()
		p.maybeUnchoke()
	case msgNotInterested:
		s.mu.Lock()
		p.theyInterested = false
		was := p.weUnchoke
		p.weUnchoke = false
		s.mu.Unlock()
		if was {
			p.send(msgChoke)
		}
	case msgHave:
		if len(payload) != 4 {
			return false
		}
		idx := u32(payload)
		s.mu.Lock()
		if s.meta == nil {
			if len(p.pendHave) < 1<<16 {
				p.pendHave = append(p.pendHave, idx)
			}
		} else if int(idx) < len(s.meta.Pieces) {
			if p.has == nil {
				p.has = make([]bool, len(s.meta.Pieces))
			}
			p.has[idx] = true
		} else {
			s.mu.Unlock()
			return false
		}
		s.mu.Unlock()
		p.fill()
	case msgBitfield:
		s.mu.Lock()
		if s.meta == nil {
			p.pendBF = append([]byte(nil), payload...)
			s.mu.Unlock()
			return true
		}
		bf, err := decodeBitfield(payload, len(s.meta.Pieces))
		if err != nil {
			s.mu.Unlock()
			return false
		}
		p.has = bf
		s.mu.Unlock()
		p.fill()
	case msgRequest:
		if len(payload) != 12 {
			return false
		}
		p.serve(int(u32(payload)), int(u32(payload[4:])), int(u32(payload[8:])))
	case msgPiece:
		if len(payload) < 8 {
			return false
		}
		return p.onPiece(int(u32(payload)), int(u32(payload[4:])), payload[8:])
	case msgCancel:
	case msgExtended:
		return p.onExtended(payload)
	}
	return true
}

// applyPendingLocked processa have/bitfield recebidos antes de conhecermos os metadados.
func (p *peer) applyPendingLocked() {
	n := len(p.s.meta.Pieces)
	p.has = make([]bool, n)
	if p.pendBF != nil {
		if bf, err := decodeBitfield(p.pendBF, n); err == nil {
			p.has = bf
		}
		p.pendBF = nil
	}
	for _, i := range p.pendHave {
		if int(i) < n {
			p.has[i] = true
		}
	}
	p.pendHave = nil
}

// afterStart envia o nosso bitfield (uma vez), e tenta pedir/desbloquear.
func (p *peer) afterStart() {
	s := p.s
	s.mu.Lock()
	if !s.started || p.bfSent {
		s.mu.Unlock()
		return
	}
	p.bfSent = true
	any := false
	for _, v := range s.servable {
		if v {
			any = true
			break
		}
	}
	var bf []byte
	if any {
		bf = encodeBitfield(s.servable)
	}
	if p.has == nil && s.meta != nil {
		p.has = make([]bool, len(s.meta.Pieces))
		p.applyPendingLocked()
	}
	s.mu.Unlock()
	if bf != nil {
		p.send(msgBitfield, bf)
	}
	p.fill()
	p.maybeUnchoke()
}

func (p *peer) maybeUnchoke() {
	s := p.s
	s.mu.Lock()
	if !s.started || !p.theyInterested || p.weUnchoke {
		s.mu.Unlock()
		return
	}
	n := 0
	for q := range s.peers {
		if q.weUnchoke {
			n++
		}
	}
	if n >= s.b.o.UploadSlots {
		s.mu.Unlock()
		return
	}
	p.weUnchoke = true
	s.mu.Unlock()
	p.send(msgUnchoke)
}

// fill atribui uma peça a este peer (se não tiver) e envia pedidos de blocos.
func (p *peer) fill() {
	s := p.s
	var reqs [][]byte
	wantInterested, wantNotInterested := false, false
	s.mu.Lock()
	if p.closed || !s.started || s.complete || s.meta == nil || p.has == nil {
		s.mu.Unlock()
		return
	}
	if p.cur == nil {
		n := len(s.meta.Pieces)
		start := randInt(n)
		for k := 0; k < n; k++ {
			i := (start + k) % n
			if s.needed[i] && !s.have[i] && s.assigned[i] == nil && p.has[i] && !p.choked {
				pl := int(s.meta.PieceLen(i))
				nb := (pl + blockSize - 1) / blockSize
				a := &assign{idx: i, buf: make([]byte, pl), got: make([]bool, nb), nblocks: nb, peer: p, last: time.Now()}
				s.assigned[i], p.cur = a, a
				break
			}
		}
		if p.cur == nil {
			// há algo que precisamos neste peer (mesmo que atribuído a outro)?
			interesting := false
			for i := range s.needed {
				if s.needed[i] && !s.have[i] && p.has[i] {
					interesting = true
					break
				}
			}
			if interesting && !p.amInterested {
				p.amInterested, wantInterested = true, true
			} else if !interesting && p.amInterested {
				p.amInterested, wantNotInterested = false, true
			}
		}
	}
	if p.cur != nil {
		if !p.amInterested {
			p.amInterested, wantInterested = true, true
		}
		a := p.cur
		for !p.choked && a.outstanding < pipeline && a.next < a.nblocks {
			begin := a.next * blockSize
			length := len(a.buf) - begin
			if length > blockSize {
				length = blockSize
			}
			r := append(append(put32(uint32(a.idx)), put32(uint32(begin))...), put32(uint32(length))...)
			reqs = append(reqs, r)
			a.next++
			a.outstanding++
		}
	}
	s.mu.Unlock()
	if wantInterested {
		p.send(msgInterested)
	}
	if wantNotInterested {
		p.send(msgNotInterested)
	}
	for _, r := range reqs {
		if p.send(msgRequest, r) != nil {
			return
		}
	}
	// se não estava desbloqueado mas temos interesse, espera-se o unchoke
}

// onPiece integra um bloco; devolve false se o peer se portou mal.
func (p *peer) onPiece(idx, begin int, data []byte) bool {
	s := p.s
	s.mu.Lock()
	a := p.cur
	if a == nil || a.idx != idx || a.verifying { // bloco tardio de uma atribuição libertada: ignorar
		s.mu.Unlock()
		return true
	}
	if begin < 0 || begin%blockSize != 0 || begin/blockSize >= a.nblocks {
		s.mu.Unlock()
		return false
	}
	blk := begin / blockSize
	want := len(a.buf) - begin
	if want > blockSize {
		want = blockSize
	}
	if len(data) != want {
		s.mu.Unlock()
		return false
	}
	if a.got[blk] {
		s.mu.Unlock()
		return true
	}
	copy(a.buf[begin:], data)
	a.got[blk] = true
	a.have++
	a.outstanding--
	a.last = time.Now()
	if a.have < a.nblocks {
		s.mu.Unlock()
		p.fill()
		return true
	}
	a.verifying = true
	m := s.meta
	s.mu.Unlock()

	// verificação SHA-1 e escrita FORA do lock
	sum := sha1.Sum(a.buf)
	ok := sum == m.Pieces[idx]
	var werr error
	if ok {
		werr = s.writePiece(m, idx, a.buf)
	}

	s.mu.Lock()
	delete(s.assigned, idx)
	p.cur = nil
	if !ok {
		p.bad++
		bad := p.bad
		s.mu.Unlock()
		s.kick()
		return bad < maxBadPieces // peer que envia lixo repetidamente é expulso
	}
	if werr != nil {
		s.failLocked(werr)
		s.mu.Unlock()
		return false
	}
	s.have[idx] = true
	s.doneBytes += s.selBytes[idx]
	serv := s.fullySelectedLocked(idx)
	s.servable[idx] = serv
	s.checkCompleteLocked()
	peers := s.peerListLocked()
	s.mu.Unlock()
	if serv {
		for _, q := range peers {
			go q.send(msgHave, put32(uint32(idx)))
		}
	}
	p.fill()
	return true
}

// writePiece grava apenas os bytes pertencentes a ficheiros selecionados.
func (s *session) writePiece(m *torrent.Meta, idx int, buf []byte) error {
	start := int64(idx) * m.PieceLength
	for _, sp := range m.PieceSpans(idx) {
		s.mu.Lock()
		f := s.files[sp.File.Index]
		s.mu.Unlock()
		if f == nil { // ficheiro não selecionado
			continue
		}
		if _, err := f.WriteAt(buf[sp.From-start:sp.To-start], sp.FileOffset); err != nil {
			return err
		}
	}
	return nil
}

// serve envia um bloco a um peer que desbloqueámos (upload).
func (p *peer) serve(idx, begin, length int) {
	s := p.s
	s.mu.Lock()
	ok := s.started && s.meta != nil && p.weUnchoke && idx >= 0 && idx < len(s.meta.Pieces) &&
		s.servable[idx] && length > 0 && length <= maxBlockReq && begin >= 0 &&
		int64(begin+length) <= s.meta.PieceLen(idx)
	var m *torrent.Meta
	if ok {
		m = s.meta
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	buf := make([]byte, length)
	start := int64(idx)*m.PieceLength + int64(begin)
	end := start + int64(length)
	for _, sp := range m.PieceSpans(idx) {
		from, to := max64(start, sp.From), min64(end, sp.To)
		if from >= to {
			continue
		}
		s.mu.Lock()
		f := s.files[sp.File.Index]
		s.mu.Unlock()
		if f == nil {
			return
		}
		if _, err := f.ReadAt(buf[from-start:to-start], sp.FileOffset+(from-sp.From)); err != nil {
			return
		}
	}
	p.send(msgPiece, put32(uint32(idx)), put32(uint32(begin)), buf)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
