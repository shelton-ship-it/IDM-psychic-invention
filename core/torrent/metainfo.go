// Package torrent contém a parte do BitTorrent que NÃO depende de rede nem de
// bibliotecas externas: leitura/validação de metainfo (BEP 3), info-hash,
// caminhos seguros, geração de fixtures e verificação por peças. O motor real
// (anacrolix/torrent) é um Backend atrás de torrentengine.
package torrent

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// MaxMetaSize limita o tamanho de um .torrent aceite (anti-abuso de memória).
const MaxMetaSize = 8 << 20

// File é um ficheiro do torrent com caminho RELATIVO já validado.
type File struct {
	Index  int
	Path   string // separador "/", sem "..", sem componentes vazios
	Length int64
}

// Meta é o metainfo BitTorrent v1 validado.
type Meta struct {
	Name        string
	InfoHash    string // SHA-1 hex minúsculo do dicionário info
	PieceLength int64
	Pieces      [][20]byte
	Files       []File // 1 elemento em torrents de ficheiro único
	Total       int64
	MultiFile   bool
	RawInfo     []byte // bytes exatos do dicionário info (base do info-hash; servidos via ut_metadata)
}

// ErrUnsupported marca metainfo legítimo mas fora do âmbito (ex.: BitTorrent v2 puro).
var ErrUnsupported = errors.New("torrent: formato não suportado")

// Parse valida e interpreta um .torrent. Recusa tudo o que seja ambíguo ou
// perigoso: caminhos com "..", absolutos, vazios, NUL, tamanhos inconsistentes.
func Parse(data []byte) (*Meta, error) {
	if len(data) == 0 || len(data) > MaxMetaSize {
		return nil, fmt.Errorf("torrent: metainfo com tamanho inválido (%d bytes)", len(data))
	}
	d := &decoder{data: data}
	v, n, err := d.value(0, 0)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, fmt.Errorf("%w: bytes extra", errBencode)
	}
	root, ok := v.(map[string]any)
	if !ok || d.rawInfo == nil {
		return nil, errors.New("torrent: sem dicionário info")
	}
	info, ok := root["info"].(map[string]any)
	if !ok {
		return nil, errors.New("torrent: info inválido")
	}
	piecesRaw, ok := info["pieces"].([]byte)
	if !ok {
		return nil, fmt.Errorf("%w: sem 'pieces' (BitTorrent v2 puro?)", ErrUnsupported)
	}
	if len(piecesRaw) == 0 || len(piecesRaw)%20 != 0 {
		return nil, errors.New("torrent: 'pieces' com tamanho inválido")
	}
	pl, ok := info["piece length"].(int64)
	if !ok || pl <= 0 || pl > 1<<30 {
		return nil, errors.New("torrent: 'piece length' inválido")
	}
	nameB, ok := info["name"].([]byte)
	if !ok {
		return nil, errors.New("torrent: sem 'name'")
	}
	name := string(nameB)
	if err := checkComponent(name); err != nil {
		return nil, fmt.Errorf("torrent: nome inseguro: %w", err)
	}
	m := &Meta{Name: name, PieceLength: pl, RawInfo: append([]byte(nil), d.rawInfo...)}
	sum := sha1.Sum(d.rawInfo)
	m.InfoHash = hex.EncodeToString(sum[:])
	for i := 0; i < len(piecesRaw); i += 20 {
		var p [20]byte
		copy(p[:], piecesRaw[i:i+20])
		m.Pieces = append(m.Pieces, p)
	}

	if lst, multi := info["files"].([]any); multi {
		m.MultiFile = true
		if len(lst) == 0 {
			return nil, errors.New("torrent: lista de ficheiros vazia")
		}
		seen := map[string]bool{}
		for i, e := range lst {
			fm, ok := e.(map[string]any)
			if !ok {
				return nil, errors.New("torrent: entrada de ficheiro inválida")
			}
			length, ok := fm["length"].(int64)
			if !ok || length < 0 {
				return nil, errors.New("torrent: 'length' inválido")
			}
			parts, ok := fm["path"].([]any)
			if !ok || len(parts) == 0 {
				return nil, errors.New("torrent: 'path' inválido")
			}
			var comps []string
			for _, pc := range parts {
				pb, ok := pc.([]byte)
				if !ok {
					return nil, errors.New("torrent: componente de path inválido")
				}
				if err := checkComponent(string(pb)); err != nil {
					return nil, fmt.Errorf("torrent: caminho inseguro %q: %w", string(pb), err)
				}
				comps = append(comps, string(pb))
			}
			p := strings.Join(comps, "/")
			if seen[strings.ToLower(p)] {
				return nil, fmt.Errorf("torrent: caminho duplicado %q", p)
			}
			seen[strings.ToLower(p)] = true
			m.Files = append(m.Files, File{Index: i, Path: p, Length: length})
			m.Total += length
		}
	} else {
		length, ok := info["length"].(int64)
		if !ok || length < 0 {
			return nil, errors.New("torrent: 'length' inválido")
		}
		m.Files = []File{{Index: 0, Path: name, Length: length}}
		m.Total = length
	}
	want := (m.Total + pl - 1) / pl
	if int64(len(m.Pieces)) != want {
		return nil, fmt.Errorf("torrent: %d peças mas o tamanho total exige %d", len(m.Pieces), want)
	}
	return m, nil
}

// checkComponent rejeita componentes de caminho perigosos.
func checkComponent(c string) error {
	switch {
	case c == "" || c == "." || c == "..":
		return errors.New("componente vazio ou relativo")
	case strings.ContainsAny(c, "/\\\x00"):
		return errors.New("separador ou NUL no componente")
	case len(c) > 255:
		return errors.New("componente demasiado longo")
	case len(c) >= 2 && c[1] == ':':
		return errors.New("letra de unidade Windows")
	}
	return nil
}
