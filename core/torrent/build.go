package torrent

import (
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BuildFixture cria um .torrent v1 a partir de ficheiros locais. É a fixture
// própria dos testes (nunca se usa rede de terceiros): files são caminhos
// relativos a base, na ordem do fluxo. Um único ficheiro => modo single-file
// (name = nome do ficheiro); vários => multi-file com name como pasta raiz.
func BuildFixture(base, name string, files []string, pieceLength int64) ([]byte, error) {
	if pieceLength <= 0 {
		return nil, fmt.Errorf("torrent: piece length inválido")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("torrent: sem ficheiros")
	}
	var stream []byte
	var pieces []byte
	flush := func(final bool) {
		for int64(len(stream)) >= pieceLength || (final && len(stream) > 0) {
			n := int64(len(stream))
			if n > pieceLength {
				n = pieceLength
			}
			sum := sha1.Sum(stream[:n])
			pieces = append(pieces, sum[:]...)
			stream = stream[n:]
		}
	}
	var entries []any
	var total int64
	for _, rel := range files {
		f, err := os.Open(filepath.Join(base, rel))
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		stream = append(stream, data...)
		flush(false)
		var parts []any
		for _, c := range splitSlash(rel) {
			parts = append(parts, c)
		}
		entries = append(entries, map[string]any{"length": int64(len(data)), "path": parts})
	}
	flush(true)
	info := map[string]any{"name": name, "piece length": pieceLength, "pieces": pieces}
	if len(files) == 1 {
		info["length"] = total
	} else {
		info["files"] = entries
	}
	return Encode(map[string]any{"info": info})
}

func splitSlash(p string) []string {
	var out []string
	cur := ""
	for _, r := range filepath.ToSlash(p) {
		if r == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
