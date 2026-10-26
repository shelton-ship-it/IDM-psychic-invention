package torrent

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// diskPath devolve o caminho de um ficheiro do torrent sob root, com defesa
// em profundidade: o resultado TEM de ficar dentro de root.
func (m *Meta) diskPath(root string, f File) (string, error) {
	var p string
	if m.MultiFile {
		p = filepath.Join(root, m.Name, filepath.FromSlash(f.Path))
	} else {
		p = filepath.Join(root, m.Name)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("torrent: caminho fora da pasta de destino: %q", f.Path)
	}
	return p, nil
}

// DiskPath devolve o caminho em disco de um ficheiro do torrent sob root,
// garantido dentro de root.
func (m *Meta) DiskPath(root string, f File) (string, error) { return m.diskPath(root, f) }

// PieceLen devolve o tamanho da peça i (a última pode ser menor).
func (m *Meta) PieceLen(i int) int64 {
	start, end := m.pieceSpan(i)
	return end - start
}

// FilesOfPiece devolve, para a peça i, cada ficheiro tocado com o intervalo
// [from,to) em coordenadas do fluxo e o offset dentro do ficheiro.
type FileSpan struct {
	File       File
	From, To   int64 // posição no fluxo contínuo
	FileOffset int64 // offset dentro do ficheiro
}

func (m *Meta) PieceSpans(i int) []FileSpan {
	start, end := m.pieceSpan(i)
	var out []FileSpan
	var off int64
	for _, f := range m.Files {
		fs, fe := off, off+f.Length
		off = fe
		if f.Length == 0 || fs >= end || fe <= start {
			continue
		}
		from, to := max64(start, fs), min64(end, fe)
		out = append(out, FileSpan{File: f, From: from, To: to, FileOffset: from - fs})
	}
	return out
}

// VerifyPiece confere UMA peça no disco (false se faltar ou divergir).
func VerifyPiece(root string, m *Meta, i int) (bool, error) {
	if i < 0 || i >= len(m.Pieces) {
		return false, fmt.Errorf("torrent: peça %d inexistente", i)
	}
	start, end := m.pieceSpan(i)
	piece := make([]byte, end-start)
	for _, sp := range m.PieceSpans(i) {
		p, err := m.diskPath(root, sp.File)
		if err != nil {
			return false, err
		}
		h, err := os.Open(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		_, rerr := h.ReadAt(piece[sp.From-start:sp.To-start], sp.FileOffset)
		h.Close()
		if rerr != nil { // ficheiro curto => ainda não está completo
			return false, nil
		}
	}
	sum := sha1.Sum(piece)
	return bytes.Equal(sum[:], m.Pieces[i][:]), nil
}

// ContentRoot devolve o caminho que representa o conteúdo no disco (ficheiro
// único ou pasta), útil para eventos de conclusão.
func (m *Meta) ContentRoot(root string) string { return filepath.Join(root, m.Name) }

// pieceFiles diz que ficheiros uma peça toca, para verificação parcial.
func (m *Meta) pieceSpan(i int) (start, end int64) {
	start = int64(i) * m.PieceLength
	end = start + m.PieceLength
	if end > m.Total {
		end = m.Total
	}
	return
}

// VerifyPieces confere as peças contra os hashes do metainfo. selected == nil
// verifica tudo; caso contrário só as peças cujos bytes pertencem INTEIRAMENTE
// a ficheiros selecionados (peças que cruzam ficheiros não selecionados não
// podem ser provadas e são ignoradas). Devolve os índices das peças inválidas
// ou em falta; verified é o número de peças realmente confirmadas.
func VerifyPieces(root string, m *Meta, selected map[int]bool) (bad []int, verified int, err error) {
	// offsets de cada ficheiro no fluxo contínuo
	offs := make([]int64, len(m.Files)+1)
	for i, f := range m.Files {
		offs[i+1] = offs[i] + f.Length
	}
	handles := map[int]*os.File{}
	defer func() {
		for _, h := range handles {
			h.Close()
		}
	}()
	open := func(i int) (*os.File, error) {
		if h, ok := handles[i]; ok {
			return h, nil
		}
		p, perr := m.diskPath(root, m.Files[i])
		if perr != nil {
			return nil, perr
		}
		h, oerr := os.Open(p)
		if oerr != nil {
			return nil, oerr
		}
		handles[i] = h
		return h, nil
	}
	buf := make([]byte, m.PieceLength)
	for pi := range m.Pieces {
		start, end := m.pieceSpan(pi)
		// ficheiros que esta peça toca
		var touched []int
		for i, f := range m.Files {
			if f.Length > 0 && offs[i] < end && offs[i+1] > start {
				touched = append(touched, i)
			}
		}
		if selected != nil {
			all := true
			for _, i := range touched {
				if !selected[i] {
					all = false
				}
			}
			if !all {
				continue
			}
		}
		piece := buf[:end-start]
		ok := true
		pos := int64(0)
		for _, i := range touched {
			h, oerr := open(i)
			if oerr != nil {
				if errors.Is(oerr, os.ErrNotExist) {
					ok = false
					break
				}
				return nil, 0, oerr
			}
			fs, fe := offs[i], offs[i+1]
			from := max64(start, fs)
			to := min64(end, fe)
			if _, serr := h.Seek(from-fs, io.SeekStart); serr != nil {
				return nil, 0, serr
			}
			n, rerr := io.ReadFull(h, piece[pos:pos+(to-from)])
			pos += int64(n)
			if rerr != nil { // ficheiro mais curto que o declarado => peça em falta
				ok = false
				break
			}
		}
		if ok {
			sum := sha1.Sum(piece)
			ok = bytes.Equal(sum[:], m.Pieces[pi][:])
		}
		if ok {
			verified++
		} else {
			bad = append(bad, pi)
		}
	}
	return bad, verified, nil
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
