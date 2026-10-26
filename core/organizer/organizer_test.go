package organizer

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, s string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCategorize(t *testing.T) {
	cases := map[[2]string]string{
		{"Filme.MKV", ""}:                  CatVideo,
		{"a.flac", ""}:                     CatAudio,
		{"x.png", ""}:                      CatImage,
		{"r.pdf", ""}:                      CatDocument,
		{"p.zip", ""}:                      CatArchive,
		{"setup.exe", ""}:                  CatSoftware,
		{"a.torrent", ""}:                  CatTorrent,
		{"semext", "video/mp4; charset=x"}: CatVideo,
		{"semext", ""}:                     CatOther,
	}
	for in, want := range cases {
		if got := Categorize(in[0], in[1]); got != want {
			t.Errorf("Categorize(%q,%q)=%s, esperado %s", in[0], in[1], got, want)
		}
	}
}

func TestMovesIntoCategoryWithoutOverwriting(t *testing.T) {
	base := t.TempDir()
	// já existe um ficheiro com o mesmo nome no destino: não pode ser sobrescrito
	existing := write(t, filepath.Join(base, CatVideo, "f.mp4"), "antigo")
	src := write(t, filepath.Join(base, "incoming", "f.mp4"), "novo")
	res, err := Organize(src, "", "", Options{BaseDir: base})
	if err != nil || res.Action != ActionMoved {
		t.Fatalf("%+v %v", res, err)
	}
	if res.NewPath == existing {
		t.Fatal("sobrescreveu ficheiro existente")
	}
	if b, _ := os.ReadFile(existing); string(b) != "antigo" {
		t.Fatal("conteúdo original alterado")
	}
	if b, _ := os.ReadFile(res.NewPath); string(b) != "novo" {
		t.Fatal("conteúdo movido incorreto")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("origem deveria ter sido movida")
	}
}

func TestDuplicateIsReportedNotDeletedNorMoved(t *testing.T) {
	base := t.TempDir()
	orig := write(t, filepath.Join(base, CatDocument, "a.pdf"), "conteudo")
	dup := write(t, filepath.Join(base, "incoming", "a-copia.pdf"), "conteudo")
	lookup := func(sha string) ([]Known, error) {
		if sha != "abc" {
			t.Errorf("hash deveria chegar em minúsculas, veio %q", sha)
		}
		return []Known{{Path: orig, SizeBytes: 8}, {Path: dup, SizeBytes: 8}, {Path: filepath.Join(base, "apagado.pdf"), SizeBytes: 8}}, nil
	}
	res, err := Organize(dup, "", "ABC", Options{BaseDir: base, Lookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionDuplicate || len(res.DuplicateOf) != 1 || res.DuplicateOf[0].Path != orig {
		t.Fatalf("duplicado mal reportado: %+v", res)
	}
	if _, err := os.Stat(dup); err != nil {
		t.Error("o ficheiro novo não pode ser movido nem apagado sem decisão do utilizador")
	}
	// com MoveDuplicates o utilizador já decidiu manter ambos
	res, err = Organize(dup, "", "abc", Options{BaseDir: base, Lookup: lookup, MoveDuplicates: true})
	if err != nil || res.Action != ActionMoved || len(res.DuplicateOf) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAlreadyInPlaceAndValidation(t *testing.T) {
	base := t.TempDir()
	p := write(t, filepath.Join(base, CatAudio, "m.mp3"), "x")
	res, err := Organize(p, "", "", Options{BaseDir: base})
	if err != nil || res.NewPath != p {
		t.Fatalf("já no sítio: %+v %v", res, err)
	}
	if _, err := Organize(p, "", "", Options{}); err == nil {
		t.Error("BaseDir vazio deve falhar")
	}
	if _, err := Organize(filepath.Join(base, "nao-existe"), "", "", Options{BaseDir: base}); err == nil {
		t.Error("ficheiro inexistente deve falhar")
	}
	if _, err := Organize(base, "", "", Options{BaseDir: base}); err == nil {
		t.Error("pastas não são organizadas")
	}
}
