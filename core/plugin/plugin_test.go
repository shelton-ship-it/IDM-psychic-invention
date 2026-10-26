package plugin

import (
	"path/filepath"
	"testing"

	"streamvault/core/domain"
	"streamvault/core/linkrec"
)

type fakeAdapter struct {
	name, version string
	schemes       []string
	restr         []string
	kind          domain.SourceKind
	panicParse    bool
}

func (f *fakeAdapter) Name() string           { return f.name }
func (f *fakeAdapter) Version() string        { return f.version }
func (f *fakeAdapter) Schemes() []string      { return f.schemes }
func (f *fakeAdapter) Restrictions() []string { return f.restr }
func (f *fakeAdapter) CanRecognize(raw string) bool {
	return len(raw) > 6 && raw[:6] == "demo:/"
}
func (f *fakeAdapter) Parse(raw string) (domain.DownloadSource, error) {
	if f.panicParse {
		panic("boom")
	}
	return domain.DownloadSource{Kind: f.kind, URL: "https://cdn.example/" + raw[6:]}, nil
}

func newFake() *fakeAdapter {
	return &fakeAdapter{name: "demo", version: "1.0.0", schemes: []string{"demo"},
		restr: []string{"Só conteúdo público sem DRM."}, kind: domain.SourceHTTP}
}

func newHost(t *testing.T) (*Host, *MemApprovals) {
	t.Helper()
	base := linkrec.NewRegistry()
	base.Register(linkrec.NewHTTPAdapter())
	ap := NewMemApprovals()
	return NewHost(base, ap), ap
}

func TestUnapprovedPluginBlocksInsteadOfFallingBack(t *testing.T) {
	h, _ := newHost(t)
	if err := h.Register(newFake()); err != nil {
		t.Fatal(err)
	}
	_, err := h.Recognize("demo://video/1")
	de, ok := err.(*domain.DownloadError)
	if !ok || de.Class != domain.ErrPolicyViolation || de.Action == "" {
		t.Fatalf("esperava bloqueio explicado, obtido %#v", err)
	}
}

func TestApprovedPluginParsesAndIsTagged(t *testing.T) {
	h, _ := newHost(t)
	h.Register(newFake())
	if err := h.Approve("demo"); err != nil {
		t.Fatal(err)
	}
	src, err := h.Recognize("demo://video/1")
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != domain.SourceHTTP || src.Metadata["adapter"] != "demo@1.0.0" {
		t.Fatalf("origem inesperada: %+v", src)
	}
	// links que o plugin não reconhece vão para o núcleo
	if s, err := h.Recognize("https://e.com/a.bin"); err != nil || s.Kind != domain.SourceHTTP {
		t.Fatalf("fallback ao núcleo falhou: %+v %v", s, err)
	}
}

func TestApprovalInvalidatedWhenAdapterChanges(t *testing.T) {
	h, ap := newHost(t)
	a := newFake()
	h.Register(a)
	h.Approve("demo")
	a.restr = append(a.restr, "Nova limitação")
	if _, err := h.Recognize("demo://x/1"); err == nil {
		t.Fatal("aprovação deveria ter caducado após mudar o adaptador")
	}
	if ap.IsApproved("demo", Fingerprint(a)) {
		t.Fatal("impressão digital nova não pode estar aprovada")
	}
}

func TestRegisterValidation(t *testing.T) {
	h, _ := newHost(t)
	noRestr := newFake()
	noRestr.restr = nil
	if h.Register(noRestr) == nil {
		t.Error("adaptador sem Restrictions deve ser recusado")
	}
	steals := newFake()
	steals.schemes = []string{"https"}
	if h.Register(steals) == nil {
		t.Error("plugin não pode reclamar esquema do núcleo")
	}
	if h.Register(newFake()) != nil || h.Register(newFake()) == nil {
		t.Error("nome duplicado deve ser recusado")
	}
}

func TestMisbehavingAdapterIsContained(t *testing.T) {
	h, _ := newHost(t)
	p := newFake()
	p.panicParse = true
	h.Register(p)
	h.Approve("demo")
	if _, err := h.Recognize("demo://x/1"); err == nil {
		t.Fatal("panic do adaptador deve virar erro tipado")
	}
	h2, _ := newHost(t)
	bad := newFake()
	bad.kind = domain.SourceUnknown
	h2.Register(bad)
	h2.Approve("demo")
	if _, err := h2.Recognize("demo://x/1"); err == nil {
		t.Fatal("origem de tipo desconhecido deve ser recusada")
	}
}

func TestFileApprovalsPersistAndFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "approvals.json")
	f, err := OpenFileApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Approve("demo", "abc")
	f2, err := OpenFileApprovals(path)
	if err != nil || !f2.IsApproved("demo", "abc") || f2.IsApproved("demo", "zzz") {
		t.Fatalf("persistência falhou: %v", err)
	}
	f2.Revoke("demo")
	f3, _ := OpenFileApprovals(path)
	if f3.IsApproved("demo", "abc") {
		t.Error("revogação não persistiu")
	}
	// ficheiro corrompido => erro (nunca "aprovado por omissão")
	bad := filepath.Join(t.TempDir(), "bad.json")
	writeFile(t, bad, "{não é json")
	if _, err := OpenFileApprovals(bad); err == nil {
		t.Error("ficheiro corrompido deve dar erro")
	}
}

// Adaptador do próprio produto: funciona sem aprovação, é revogável, e a
// revogação sobrevive ao reinício; os de terceiros continuam a exigir aprovação.
func TestBuiltinAdapterWorksByDefaultAndIsRevocable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ap.json")
	mk := func() (*Host, *FileApprovals) {
		ap, err := OpenFileApprovals(path)
		if err != nil {
			t.Fatal(err)
		}
		base := linkrec.NewRegistry()
		base.Register(linkrec.NewHTTPAdapter())
		h := NewHost(base, ap)
		if err := h.RegisterBuiltin(newFake()); err != nil {
			t.Fatal(err)
		}
		return h, ap
	}
	h, _ := mk()
	if _, err := h.Recognize("demo://v/1"); err != nil {
		t.Fatalf("adaptador próprio deve funcionar sem aprovação: %v", err)
	}
	if l := h.List(); len(l) != 1 || !l[0].Approved || !l[0].Builtin {
		t.Fatalf("List: %+v", l)
	}
	if err := h.Revoke("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Recognize("demo://v/1"); err == nil {
		t.Fatal("adaptador revogado deve parar")
	}
	h2, _ := mk() // "reinício"
	if _, err := h2.Recognize("demo://v/1"); err == nil {
		t.Fatal("a revogação deve persistir após reiniciar")
	}
	if err := h2.Approve("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.Recognize("demo://v/1"); err != nil {
		t.Fatalf("voltar a aprovar deve reativar: %v", err)
	}
	// terceiros: continuam bloqueados até aprovação
	h3, _ := newHost(t)
	h3.Register(newFake())
	if _, err := h3.Recognize("demo://v/1"); err == nil {
		t.Fatal("adaptador de terceiros não pode funcionar sem aprovação")
	}
}
