package btnative

import (
	"streamvault/core/torrent"
)

// sendExtHandshake anuncia ut_metadata (id local 1) e, se já soubermos, o tamanho.
func (p *peer) sendExtHandshake() {
	s := p.s
	d := map[string]any{"m": map[string]any{"ut_metadata": int64(extUTMetadata)}, "v": "StreamVault"}
	s.mu.Lock()
	if s.meta != nil {
		d["metadata_size"] = int64(len(s.meta.RawInfo))
	}
	s.mu.Unlock()
	b, err := torrent.Encode(d)
	if err != nil {
		return
	}
	p.send(msgExtended, []byte{extHandshake}, b)
}

// onExtended trata BEP 10 / BEP 9; devolve false para desligar o peer.
func (p *peer) onExtended(payload []byte) bool {
	s := p.s
	if len(payload) < 1 {
		return false
	}
	id, body := payload[0], payload[1:]
	switch id {
	case extHandshake:
		v, _, err := torrent.DecodePrefix(body)
		if err != nil {
			return false
		}
		root, _ := v.(map[string]any)
		m, _ := root["m"].(map[string]any)
		md, _ := m["ut_metadata"].(int64)
		size, _ := root["metadata_size"].(int64)
		if md <= 0 || md > 255 {
			return true // não suporta ut_metadata
		}
		s.mu.Lock()
		p.remoteMD = int(md)
		need := s.meta == nil
		s.mu.Unlock()
		if need && size > 0 && s.wantMetaSize(int(size)) {
			p.requestMetadata(int(size))
		}
	case extUTMetadata:
		v, n, err := torrent.DecodePrefix(body)
		if err != nil {
			return false
		}
		d, _ := v.(map[string]any)
		typ, _ := d["msg_type"].(int64)
		piece, _ := d["piece"].(int64)
		switch typ {
		case mdRequest:
			p.serveMetadata(int(piece))
		case mdData:
			total, _ := d["total_size"].(int64)
			return s.metaPieceDone(int(total), int(piece), body[n:])
		case mdReject:
		}
	}
	return true
}

func (p *peer) requestMetadata(size int) {
	np := (size + metadataPiece - 1) / metadataPiece
	s := p.s
	s.mu.Lock()
	md := p.remoteMD
	s.mu.Unlock()
	for i := 0; i < np; i++ {
		b, _ := torrent.Encode(map[string]any{"msg_type": int64(mdRequest), "piece": int64(i)})
		if p.send(msgExtended, []byte{byte(md)}, b) != nil {
			return
		}
	}
}

// serveMetadata responde a pedidos de metadados quando já os temos.
func (p *peer) serveMetadata(piece int) {
	s := p.s
	s.mu.Lock()
	m := s.meta
	md := p.remoteMD
	s.mu.Unlock()
	if m == nil || md == 0 {
		return
	}
	raw := m.RawInfo
	np := (len(raw) + metadataPiece - 1) / metadataPiece
	if piece < 0 || piece >= np {
		rej, _ := torrent.Encode(map[string]any{"msg_type": int64(mdReject), "piece": int64(piece)})
		p.send(msgExtended, []byte{byte(md)}, rej)
		return
	}
	from, to := piece*metadataPiece, (piece+1)*metadataPiece
	if to > len(raw) {
		to = len(raw)
	}
	hdr, _ := torrent.Encode(map[string]any{"msg_type": int64(mdData), "piece": int64(piece), "total_size": int64(len(raw))})
	p.send(msgExtended, []byte{byte(md)}, hdr, raw[from:to])
}
