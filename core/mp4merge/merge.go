// Package mp4merge junta vários ficheiros MP4 fragmentados (fMP4/CMAF), cada um
// com UMA faixa (típico: vídeo e áudio descarregados em separado de um HLS com
// #EXT-X-MEDIA ou de um DASH), num único MP4 fragmentado com todas as faixas.
//
// Não recodifica nada: copia as amostras tal como estão (remux sem perdas). Só
// reescreve o que é preciso para as faixas coexistirem: identificadores de faixa,
// números de sequência dos fragmentos e o moov combinado. Os fragmentos são
// intercalados por tempo de descodificação, em fluxo (memória limitada a um
// fragmento por faixa, não ao ficheiro inteiro).
//
// Usa a biblioteca github.com/Eyevinn/mp4ff (MIT) para ler e escrever as caixas.
// Recusa, com erro explicado: ficheiros não fragmentados, protegidos (pssh),
// com várias faixas num só ficheiro, ou com fragmentos que dependem de
// posições absolutas no ficheiro (base-data-offset).
package mp4merge

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Stats resume o que foi escrito.
type Stats struct {
	Tracks    int
	Fragments int
	Bytes     int64
}

// Merge escreve em w o MP4 combinado dos ficheiros srcs (na ordem dada: a
// primeira faixa fica com o id 1, a segunda com o 2…). Cada src é um ficheiro
// fMP4 completo: segmento de inicialização seguido dos fragmentos.
func Merge(w io.Writer, srcs []string) (Stats, error) {
	if len(srcs) == 0 {
		return Stats{}, errors.New("mp4merge: sem ficheiros para juntar")
	}
	if len(srcs) > 8 {
		return Stats{}, errors.New("mp4merge: demasiadas faixas")
	}
	var sources []*source
	defer func() {
		for _, s := range sources {
			s.f.Close()
		}
	}()
	for i, p := range srcs {
		s, err := openSource(p, uint32(i+1))
		if err != nil {
			return Stats{}, fmt.Errorf("%s: %w", baseName(p), err)
		}
		sources = append(sources, s)
	}

	moov, err := combine(sources)
	if err != nil {
		return Stats{}, err
	}
	cw := &countWriter{w: bufio.NewWriterSize(w, 256<<10)}
	st := Stats{Tracks: len(sources)}
	if err := sources[0].ftyp.Encode(cw); err != nil {
		return st, err
	}
	if err := moov.Encode(cw); err != nil {
		return st, err
	}

	var seq uint32 = 1
	for {
		// próximo fragmento por tempo de descodificação (empate: faixa anterior)
		var pick *source
		for _, s := range sources {
			if s.cur == nil {
				continue
			}
			if pick == nil || s.cur.t < pick.cur.t {
				pick = s
			}
		}
		if pick == nil {
			break
		}
		fr := pick.cur
		fr.moof.Mfhd.SequenceNumber = seq
		seq++
		fr.moof.Traf.Tfhd.TrackID = pick.newID
		if err := fr.moof.Encode(cw); err != nil {
			return st, err
		}
		if err := fr.mdat.Encode(cw); err != nil {
			return st, err
		}
		st.Fragments++
		if err := pick.advance(); err != nil {
			return st, fmt.Errorf("%s: %w", baseName(pick.path), err)
		}
	}
	if err := cw.w.Flush(); err != nil {
		return st, err
	}
	st.Bytes = cw.n
	return st, nil
}

// MergeFiles é Merge para um ficheiro de destino (criado/truncado).
func MergeFiles(dst string, srcs []string) (Stats, error) {
	f, err := os.Create(dst)
	if err != nil {
		return Stats{}, err
	}
	st, err := Merge(f, srcs)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return st, err
}

// ---- leitura de uma fonte ----

type frag struct {
	moof *mp4.MoofBox
	mdat *mp4.MdatBox
	t    float64 // tempo de descodificação, em segundos
}

type source struct {
	path  string
	f     *os.File
	br    *bufio.Reader
	pos   uint64
	ftyp  *mp4.FtypBox
	moov  *mp4.MoovBox
	ts    uint32 // escala de tempo da faixa (mdhd)
	oldID uint32
	newID uint32
	cur   *frag // próximo fragmento (nil = fim)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}

func (s *source) readBox() (mp4.Box, error) {
	b, err := mp4.DecodeBox(s.pos, s.br)
	if err != nil {
		if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("ficheiro truncado")
		}
		return nil, err
	}
	s.pos += b.Size()
	return b, nil
}

func openSource(path string, newID uint32) (*source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	s := &source{path: path, f: f, br: bufio.NewReaderSize(f, 256<<10), newID: newID}
	for s.moov == nil {
		b, err := s.readBox()
		if err == io.EOF {
			f.Close()
			return nil, errors.New("não tem segmento de inicialização (moov)")
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		switch bx := b.(type) {
		case *mp4.FtypBox:
			s.ftyp = bx
		case *mp4.MoovBox:
			s.moov = bx
		case *mp4.MoofBox:
			f.Close()
			return nil, errors.New("fragmento antes do segmento de inicialização")
		}
	}
	if s.ftyp == nil { // alguns segmentos de inicialização DASH não têm ftyp
		s.ftyp = mp4.NewFtyp("iso6", 0, []string{"iso6", "mp41", "isom"})
	}
	m := s.moov
	switch {
	case m.Mvex == nil:
		f.Close()
		return nil, errors.New("não é um MP4 fragmentado (sem mvex): não é possível juntar")
	case len(m.Traks) != 1:
		f.Close()
		return nil, fmt.Errorf("tem %d faixas (esperava 1): não é possível separar", len(m.Traks))
	case len(m.Psshs) > 0 || m.Pssh != nil:
		f.Close()
		return nil, errors.New("conteúdo protegido (pssh)")
	case m.Trak.Mdia == nil || m.Trak.Mdia.Mdhd == nil || m.Trak.Mdia.Mdhd.Timescale == 0 || m.Mvhd == nil || m.Trak.Tkhd == nil:
		f.Close()
		return nil, errors.New("segmento de inicialização incompleto")
	}
	s.ts = m.Trak.Mdia.Mdhd.Timescale
	s.oldID = m.Trak.Tkhd.TrackID
	if err := s.advance(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// advance lê o próximo par moof+mdat para s.cur (nil no fim).
func (s *source) advance() error {
	s.cur = nil
	for {
		b, err := s.readBox()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		moof, ok := b.(*mp4.MoofBox)
		if !ok {
			continue // styp, sidx, emsg, free, mfra…: não são precisos
		}
		if moof.Mfhd == nil || len(moof.Trafs) != 1 || moof.Traf == nil || moof.Traf.Tfhd == nil {
			return errors.New("fragmento com várias faixas ou incompleto")
		}
		tf := moof.Traf
		if tf.Tfhd.TrackID != s.oldID {
			return fmt.Errorf("fragmento da faixa %d num ficheiro da faixa %d", tf.Tfhd.TrackID, s.oldID)
		}
		if tf.Tfhd.HasBaseDataOffset() {
			return errors.New("fragmentos com base-data-offset absoluto não são suportados")
		}
		if tf.Tfdt == nil {
			return errors.New("fragmento sem tfdt (tempo de descodificação)")
		}
		if tf.Senc != nil || tf.UUIDSenc != nil || tf.Saiz != nil || tf.Saio != nil {
			return errors.New("conteúdo cifrado (senc)")
		}
		// o mdat segue-se ao moof (podem existir caixas livres pelo meio)
		for {
			nb, err := s.readBox()
			if err == io.EOF {
				return errors.New("fragmento sem mdat (ficheiro truncado)")
			}
			if err != nil {
				return err
			}
			if md, ok := nb.(*mp4.MdatBox); ok {
				s.cur = &frag{moof: moof, mdat: md, t: float64(tf.Tfdt.BaseMediaDecodeTime()) / float64(s.ts)}
				return nil
			}
			if _, isMoof := nb.(*mp4.MoofBox); isMoof {
				return errors.New("moof sem mdat")
			}
		}
	}
}

// ---- moov combinado ----

func combine(srcs []*source) (*mp4.MoovBox, error) {
	base := srcs[0].moov
	mvTS := base.Mvhd.Timescale
	// o mehd (duração total) deixa de ser verdade com mais faixas
	if base.Mvex != nil && base.Mvex.Mehd != nil {
		kept := base.Mvex.Children[:0]
		for _, c := range base.Mvex.Children {
			if c.Type() != "mehd" {
				kept = append(kept, c)
			}
		}
		base.Mvex.Children, base.Mvex.Mehd = kept, nil
	}
	for i, s := range srcs {
		trak := s.moov.Trak
		trak.Tkhd.TrackID = s.newID
		var trex *mp4.TrexBox
		for _, t := range s.moov.Mvex.Trexs {
			if t.TrackID == s.oldID {
				trex = t
			}
		}
		if trex == nil {
			return nil, fmt.Errorf("%s: sem trex para a faixa %d", baseName(s.path), s.oldID)
		}
		trex.TrackID = s.newID
		if i == 0 {
			continue
		}
		// listas de edição estão na escala do FILME (mvhd) de cada ficheiro: converter
		if s.moov.Mvhd.Timescale != mvTS && s.moov.Mvhd.Timescale != 0 && trak.Edts != nil {
			for _, el := range trak.Edts.Elst {
				for k := range el.Entries {
					el.Entries[k].SegmentDuration = uint64(math.Round(float64(el.Entries[k].SegmentDuration) *
						float64(mvTS) / float64(s.moov.Mvhd.Timescale)))
				}
			}
		}
		base.AddChild(trak)
		base.Mvex.AddChild(trex)
	}
	base.Mvhd.NextTrackID = uint32(len(srcs)) + 1
	return base, nil
}

type countWriter struct {
	w *bufio.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
