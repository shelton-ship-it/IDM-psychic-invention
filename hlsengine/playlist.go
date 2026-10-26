// Package hlsengine descarrega streams HLS (.m3u8) SEM cifra e em demanda (VOD),
// concatenando os segmentos num ficheiro (.ts, ou .mp4 fragmentado se houver
// EXT-X-MAP). É deliberadamente restrito (ADR-002/007/009): recusa cifra
// (#EXT-X-KEY), transmissões ao vivo e byte-ranges, em vez de
// produzir ficheiros incompletos ou contornar proteções.
package hlsengine

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	maxPlaylistBytes = 5 << 20
	maxSegments      = 20000
	maxLineLen       = 8192
)

// Variant é uma versão (resolução/débito) de uma master playlist.
type Variant struct {
	URL       string
	Bandwidth int64
	Height    int
	AudioGrp  string
}

// Segment é um segmento de mídia.
type Segment struct {
	URL      string
	Duration float64
}

// Rendition é uma faixa alternativa com playlist própria (hoje: áudio).
type Rendition struct {
	URL      string
	Name     string
	Language string
	Default  bool
}

// Playlist é o resultado da análise (master OU media).
type Playlist struct {
	Master   bool
	Variants []Variant
	// Audio: faixas de áudio com playlist própria (#EXT-X-MEDIA TYPE=AUDIO com URI),
	// por GROUP-ID. Descarregam-se à parte e juntam-se ao vídeo (ADR-011).
	Audio map[string][]Rendition

	// Ext força a extensão do ficheiro de saída (DASH: ".mp4", ".m4a", ".webm"); vazio = deduzir.
	Ext string
	// Container ("mp4" | "webm") só é preenchido em DASH; em HLS deduz-se (EXT-X-MAP = fMP4).
	Container string

	Segments  []Segment
	MapURL    string // EXT-X-MAP (fMP4)
	EndList   bool
	KeyMethod string // "" ou "NONE" = sem cifra
	ByteRange bool
}

// attrs lê uma lista de atributos HLS: CHAVE=valor,CHAVE="valor, com vírgulas".
func attrs(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.ToUpper(strings.TrimSpace(s[i : i+eq]))
		i += eq + 1
		var val string
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				val, i = s[i+1:], len(s)
			} else {
				val, i = s[i+1:i+1+end], i+1+end+1
			}
			if i < len(s) && s[i] == ',' {
				i++
			}
		} else {
			end := strings.IndexByte(s[i:], ',')
			if end < 0 {
				val, i = s[i:], len(s)
			} else {
				val, i = s[i:i+end], i+end+1
			}
		}
		if key != "" {
			if _, ok := out[key]; !ok {
				out[key] = strings.TrimSpace(val)
			}
		}
	}
	return out
}

// Parse analisa o texto de uma playlist; os URLs relativos resolvem-se contra base.
func Parse(text string, base *url.URL) (*Playlist, error) {
	if len(text) > maxPlaylistBytes {
		return nil, errors.New("playlist demasiado grande")
	}
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 200000 {
		return nil, errors.New("playlist com demasiadas linhas")
	}
	first := true
	p := &Playlist{Audio: map[string][]Rendition{}}
	resolve := func(ref string) (string, error) {
		if len(ref) > 4096 {
			return "", errors.New("URI demasiado longo")
		}
		u, err := base.Parse(ref)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("URI não suportado: %q", truncate(ref, 60))
		}
		return u.String(), nil
	}
	var pendingVariant *Variant
	var pendingDur float64 = -1
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if len(line) > maxLineLen {
			return nil, errors.New("linha demasiado longa na playlist")
		}
		if first {
			if line != "#EXTM3U" {
				return nil, errors.New("não é uma playlist HLS (falta #EXTM3U)")
			}
			first = false
			continue
		}
		if !strings.HasPrefix(line, "#") { // URI do segmento ou da variante
			uri, err := resolve(line)
			if err != nil {
				return nil, err
			}
			switch {
			case pendingVariant != nil:
				pendingVariant.URL = uri
				p.Variants = append(p.Variants, *pendingVariant)
				pendingVariant = nil
			case pendingDur >= 0:
				if len(p.Segments) >= maxSegments {
					return nil, fmt.Errorf("playlist com mais de %d segmentos", maxSegments)
				}
				p.Segments = append(p.Segments, Segment{URL: uri, Duration: pendingDur})
				pendingDur = -1
			}
			continue
		}
		tag, rest, _ := strings.Cut(line, ":")
		switch tag {
		case "#EXT-X-STREAM-INF":
			a := attrs(rest)
			v := Variant{AudioGrp: a["AUDIO"]}
			v.Bandwidth, _ = strconv.ParseInt(a["BANDWIDTH"], 10, 64)
			if res := a["RESOLUTION"]; res != "" {
				if _, h, ok := strings.Cut(strings.ToLower(res), "x"); ok {
					v.Height, _ = strconv.Atoi(h)
				}
			}
			p.Master = true
			pendingVariant = &v
		case "#EXT-X-MEDIA":
			a := attrs(rest)
			if strings.EqualFold(a["TYPE"], "AUDIO") && a["URI"] != "" {
				uri, err := resolve(a["URI"])
				if err != nil {
					return nil, err
				}
				g := a["GROUP-ID"]
				if len(p.Audio[g]) < 32 { // limite anti-abuso
					p.Audio[g] = append(p.Audio[g], Rendition{URL: uri, Name: truncate(a["NAME"], 80),
						Language: truncate(a["LANGUAGE"], 16), Default: strings.EqualFold(a["DEFAULT"], "YES")})
				}
			}
		case "#EXTINF":
			d, _, _ := strings.Cut(rest, ",")
			f, err := strconv.ParseFloat(strings.TrimSpace(d), 64)
			if err != nil || f < 0 {
				f = 0
			}
			pendingDur = f
		case "#EXT-X-KEY":
			m := strings.ToUpper(attrs(rest)["METHOD"])
			if m != "" && m != "NONE" {
				p.KeyMethod = m
			}
		case "#EXT-X-MAP":
			a := attrs(rest)
			if a["BYTERANGE"] != "" {
				p.ByteRange = true
			}
			if a["URI"] != "" {
				uri, err := resolve(a["URI"])
				if err != nil {
					return nil, err
				}
				p.MapURL = uri
			}
		case "#EXT-X-BYTERANGE":
			p.ByteRange = true
		case "#EXT-X-ENDLIST":
			p.EndList = true
		}
	}
	if first {
		return nil, errors.New("playlist vazia")
	}
	return p, nil
}

// pickVariant escolhe a melhor variante (maior débito; empate: maior altura).
func (p *Playlist) pickVariant() (*Variant, error) {
	if len(p.Variants) == 0 {
		return nil, errors.New("master playlist sem variantes")
	}
	vs := append([]Variant(nil), p.Variants...)
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Bandwidth != vs[j].Bandwidth {
			return vs[i].Bandwidth > vs[j].Bandwidth
		}
		return vs[i].Height > vs[j].Height
	})
	best := vs[0]
	return &best, nil
}

// pickAudio escolhe a faixa de áudio em separado da variante: a marcada DEFAULT=YES
// ou, na falta, a primeira. nil se o áudio já vem na própria variante.
func (p *Playlist) pickAudio(v *Variant) *Rendition {
	if v == nil || v.AudioGrp == "" {
		return nil
	}
	rs := p.Audio[v.AudioGrp]
	for i := range rs {
		if rs[i].Default {
			return &rs[i]
		}
	}
	if len(rs) > 0 {
		return &rs[0]
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
