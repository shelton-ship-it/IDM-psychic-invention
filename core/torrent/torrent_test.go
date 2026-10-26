package torrent

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, p string, n int, seed byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// multi cria 3 ficheiros (tamanhos que NÃO alinham com a peça) e o seu .torrent.
func multi(t *testing.T) (src string, meta *Meta, raw []byte) {
	t.Helper()
	src = t.TempDir()
	writeFile(t, filepath.Join(src, "a.bin"), 10_000, 1)
	writeFile(t, filepath.Join(src, "sub", "b.bin"), 7_777, 2)
	writeFile(t, filepath.Join(src, "c.txt"), 4_321, 3)
	raw, err := BuildFixture(src, "pack", []string{"a.bin", "sub/b.bin", "c.txt"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	meta, err = Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// materializa o "download" em dl/<name>/... a partir da origem.
func materialize(t *testing.T, src string, m *Meta) string {
	t.Helper()
	dl := t.TempDir()
	for _, f := range m.Files {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		p, _ := m.diskPath(dl, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
	}
	return dl
}

func TestBencodeRoundTripAndStrictness(t *testing.T) {
	v := map[string]any{"a": int64(-5), "b": []byte("xyz"), "c": []any{int64(1), []byte("")}, "d": map[string]any{"k": int64(0)}}
	enc, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	re, _ := Encode(dec)
	if !bytes.Equal(enc, re) {
		t.Fatalf("round-trip diverge: %q vs %q", enc, re)
	}
	bad := []string{"i03e", "i-0e", "ie", "i1", "5:ab", "d1:b i1e1:a i2ee", "d1:ai1e1:ai2ee", "l", "x", "i1ee", "01:a", "d3:fooe"}
	for _, s := range bad {
		if _, err := Decode([]byte(strings.ReplaceAll(s, " ", ""))); err == nil {
			t.Errorf("deveria rejeitar %q", s)
		}
	}
	deep := strings.Repeat("l", 100) + strings.Repeat("e", 100)
	if _, err := Decode([]byte(deep)); err == nil {
		t.Error("profundidade excessiva deveria falhar")
	}
	// string com comprimento absurdo não pode causar overflow/alocação
	if _, err := Decode([]byte("99999999999999999999:a")); err == nil {
		t.Error("comprimento absurdo deveria falhar")
	}
}

func TestParseMultiFileAndInfoHash(t *testing.T) {
	_, m, raw := multi(t)
	if !m.MultiFile || len(m.Files) != 3 || m.Total != 10_000+7_777+4_321 {
		t.Fatalf("meta inesperado: %+v", m)
	}
	if m.Files[1].Path != "sub/b.bin" {
		t.Errorf("caminho: %q", m.Files[1].Path)
	}
	// info-hash independente: SHA-1 da codificação canónica do dicionário info
	root, _ := Decode(raw)
	infoEnc, _ := Encode(root.(map[string]any)["info"])
	sum := sha1.Sum(infoEnc)
	if m.InfoHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("info-hash %s != %s", m.InfoHash, hex.EncodeToString(sum[:]))
	}
}

func TestParseSingleFile(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "one.bin"), 9000, 9)
	raw, err := BuildFixture(src, "one.bin", []string{"one.bin"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(raw)
	if err != nil || m.MultiFile || len(m.Files) != 1 || m.Files[0].Path != "one.bin" || len(m.Pieces) != 3 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestParseRejectsHostileMetainfo(t *testing.T) {
	mk := func(name string, files []any, extra map[string]any) []byte {
		info := map[string]any{"name": name, "piece length": int64(16384), "pieces": bytes.Repeat([]byte{1}, 20)}
		if files != nil {
			info["files"] = files
		} else {
			info["length"] = int64(100)
		}
		for k, v := range extra {
			info[k] = v
		}
		b, _ := Encode(map[string]any{"info": info})
		return b
	}
	file := func(parts ...string) any {
		var ps []any
		for _, p := range parts {
			ps = append(ps, p)
		}
		return map[string]any{"length": int64(100), "path": ps}
	}
	cases := map[string][]byte{
		"nome com ..":              mk("..", nil, nil),
		"nome com barra":           mk("a/b", nil, nil),
		"nome absoluto":            mk("/etc", nil, nil),
		"nome letra de unidade":    mk("C:x", nil, nil),
		"path com ..":              mk("p", []any{file("..", "evil")}, nil),
		"path com separador":       mk("p", []any{file("a/../b")}, nil),
		"path vazio":               mk("p", []any{file("")}, nil),
		"path duplicado":           mk("p", []any{file("A"), file("a")}, nil),
		"lista de ficheiros vazia": mk("p", []any{}, nil),
		"pieces não bate":          mk("p", nil, map[string]any{"length": int64(100_000)}),
		"sem info":                 []byte("de"),
		"lixo":                     []byte("não é bencode"),
		"v2 puro (sem pieces)": func() []byte {
			b, _ := Encode(map[string]any{"info": map[string]any{"name": "x", "piece length": int64(16384), "meta version": int64(2)}})
			return b
		}(),
	}
	for name, data := range cases {
		if _, err := Parse(data); err == nil {
			t.Errorf("%s: deveria ser rejeitado", name)
		}
	}
	if _, err := Parse(nil); err == nil {
		t.Error("vazio deve falhar")
	}
}

func TestVerifyPiecesGoodCorruptedMissing(t *testing.T) {
	src, m, _ := multi(t)
	dl := materialize(t, src, m)
	bad, ok, err := VerifyPieces(dl, m, nil)
	if err != nil || len(bad) != 0 || ok != len(m.Pieces) {
		t.Fatalf("download íntegro: bad=%v ok=%d/%d err=%v", bad, ok, len(m.Pieces), err)
	}
	// corromper 1 byte em b.bin (começa no offset 10000 → peça 2)
	p, _ := m.diskPath(dl, m.Files[1])
	data, _ := os.ReadFile(p)
	data[100] ^= 0xFF
	os.WriteFile(p, data, 0o644)
	bad, _, err = VerifyPieces(dl, m, nil)
	if err != nil || len(bad) != 1 || bad[0] != (10_000+100)/4096 {
		t.Fatalf("esperava só a peça %d inválida, obtive %v (err=%v)", (10_000+100)/4096, bad, err)
	}
	// ficheiro em falta
	os.Remove(p)
	bad, _, err = VerifyPieces(dl, m, nil)
	if err != nil || len(bad) == 0 {
		t.Fatalf("ficheiro em falta deve invalidar peças: %v %v", bad, err)
	}
	// ficheiro truncado
	os.WriteFile(p, data[:50], 0o644)
	if bad, _, _ = VerifyPieces(dl, m, nil); len(bad) == 0 {
		t.Fatal("ficheiro truncado deve invalidar peças")
	}
}

func TestVerifyPiecesSubsetOnlyProvesFullySelectedPieces(t *testing.T) {
	src, m, _ := multi(t)
	dl := materialize(t, src, m)
	// só o ficheiro 0 (a.bin, 10000 bytes): peças 0 e 1 são inteiras; a peça 2
	// (8192..12288) cruza b.bin → não pode ser provada
	bad, ok, err := VerifyPieces(dl, m, map[int]bool{0: true})
	if err != nil || len(bad) != 0 || ok != 2 {
		t.Fatalf("subset: bad=%v ok=%d err=%v (esperado ok=2)", bad, ok, err)
	}
	// ficheiros não selecionados ausentes não geram falhas
	os.Remove(filepath.Join(dl, "pack", "c.txt"))
	if bad, _, _ = VerifyPieces(dl, m, map[int]bool{0: true}); len(bad) != 0 {
		t.Fatalf("não selecionados não contam: %v", bad)
	}
}

func TestVerifySingleFileLayoutAndTraversalGuard(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "s.bin"), 5000, 5)
	raw, _ := BuildFixture(src, "s.bin", []string{"s.bin"}, 2048)
	m, _ := Parse(raw)
	dl := t.TempDir()
	data, _ := os.ReadFile(filepath.Join(src, "s.bin"))
	os.WriteFile(filepath.Join(dl, "s.bin"), data, 0o644)
	if bad, ok, err := VerifyPieces(dl, m, nil); err != nil || len(bad) != 0 || ok != 3 {
		t.Fatalf("%v %d %v", bad, ok, err)
	}
	// Meta construída à mão com path malicioso nunca sai de root
	evil := &Meta{Name: "x", MultiFile: true, PieceLength: 16, Pieces: [][20]byte{{}}, Total: 5,
		Files: []File{{Path: "../../escape", Length: 5}}}
	if _, _, err := VerifyPieces(t.TempDir(), evil, nil); err == nil {
		t.Error("caminho fora de root deve ser recusado em VerifyPieces")
	}
}
