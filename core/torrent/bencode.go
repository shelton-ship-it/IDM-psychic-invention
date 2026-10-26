package torrent

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Valores bencode: int64, []byte, []any, map[string]any.

var errBencode = errors.New("torrent: bencode inválido")

const maxDepth = 32 // evita recursão ilimitada em entradas hostis

// Encode serializa v em bencode canónico (chaves de dicionário ordenadas).
func Encode(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := enc(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func enc(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case int:
		fmt.Fprintf(b, "i%de", x)
	case int64:
		fmt.Fprintf(b, "i%de", x)
	case string:
		fmt.Fprintf(b, "%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(b, "%d:", len(x))
		b.Write(x)
	case []any:
		b.WriteByte('l')
		for _, e := range x {
			if err := enc(b, e); err != nil {
				return err
			}
		}
		b.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('d')
		for _, k := range keys {
			fmt.Fprintf(b, "%d:%s", len(k), k)
			if err := enc(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('e')
	default:
		return fmt.Errorf("torrent: tipo não codificável %T", v)
	}
	return nil
}

type decoder struct {
	data []byte
	// rawInfo guarda os bytes EXATOS do valor da chave "info" do dicionário
	// de topo — o info-hash é SHA-1 desses bytes, nunca de uma re-serialização.
	rawInfo []byte
}

// Decode lê UM valor bencode e exige que não sobrem bytes.
func Decode(data []byte) (any, error) {
	d := &decoder{data: data}
	v, n, err := d.value(0, 0)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, fmt.Errorf("%w: bytes extra após o valor", errBencode)
	}
	return v, nil
}

func (d *decoder) value(pos, depth int) (any, int, error) {
	if depth > maxDepth {
		return nil, 0, fmt.Errorf("%w: profundidade excessiva", errBencode)
	}
	if pos >= len(d.data) {
		return nil, 0, fmt.Errorf("%w: fim inesperado", errBencode)
	}
	switch c := d.data[pos]; {
	case c == 'i':
		end := bytes.IndexByte(d.data[pos:], 'e')
		if end < 0 {
			return nil, 0, fmt.Errorf("%w: inteiro sem 'e'", errBencode)
		}
		s := string(d.data[pos+1 : pos+end])
		// forma canónica: sem "+", sem zeros à esquerda, sem "-0"
		if s == "" || s == "-0" || (len(s) > 1 && s[0] == '0') || (len(s) > 2 && s[0] == '-' && s[1] == '0') {
			return nil, 0, fmt.Errorf("%w: inteiro não canónico %q", errBencode, s)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: inteiro %q", errBencode, s)
		}
		return n, pos + end + 1, nil
	case c >= '0' && c <= '9':
		colon := bytes.IndexByte(d.data[pos:], ':')
		if colon < 0 {
			return nil, 0, fmt.Errorf("%w: string sem ':'", errBencode)
		}
		ls := string(d.data[pos : pos+colon])
		if len(ls) > 1 && ls[0] == '0' {
			return nil, 0, fmt.Errorf("%w: comprimento não canónico", errBencode)
		}
		l, err := strconv.Atoi(ls)
		if err != nil || l < 0 {
			return nil, 0, fmt.Errorf("%w: comprimento %q", errBencode, ls)
		}
		start := pos + colon + 1
		if l > len(d.data)-start { // sem overflow: compara com o restante
			return nil, 0, fmt.Errorf("%w: string maior que os dados", errBencode)
		}
		return append([]byte(nil), d.data[start:start+l]...), start + l, nil
	case c == 'l':
		var list []any
		pos++
		for {
			if pos >= len(d.data) {
				return nil, 0, fmt.Errorf("%w: lista sem 'e'", errBencode)
			}
			if d.data[pos] == 'e' {
				return append([]any{}, list...), pos + 1, nil
			}
			v, n, err := d.value(pos, depth+1)
			if err != nil {
				return nil, 0, err
			}
			list, pos = append(list, v), n
		}
	case c == 'd':
		m := map[string]any{}
		pos++
		var prev string
		first := true
		for {
			if pos >= len(d.data) {
				return nil, 0, fmt.Errorf("%w: dicionário sem 'e'", errBencode)
			}
			if d.data[pos] == 'e' {
				return m, pos + 1, nil
			}
			kv, n, err := d.value(pos, depth+1)
			if err != nil {
				return nil, 0, err
			}
			kb, ok := kv.([]byte)
			if !ok {
				return nil, 0, fmt.Errorf("%w: chave não é string", errBencode)
			}
			k := string(kb)
			if !first && k <= prev {
				return nil, 0, fmt.Errorf("%w: chaves fora de ordem/duplicadas (%q)", errBencode, k)
			}
			prev, first = k, false
			vstart := n
			v, n2, err := d.value(n, depth+1)
			if err != nil {
				return nil, 0, err
			}
			if depth == 0 && k == "info" {
				d.rawInfo = d.data[vstart:n2]
			}
			m[k], pos = v, n2
		}
	}
	return nil, 0, fmt.Errorf("%w: byte inesperado %q na posição %d", errBencode, d.data[pos], pos)
}

// DecodePrefix lê UM valor bencode no início de data e devolve quantos bytes
// consumiu (necessário para mensagens ut_metadata: dicionário + bytes brutos).
func DecodePrefix(data []byte) (any, int, error) {
	d := &decoder{data: data}
	return d.value(0, 0)
}
