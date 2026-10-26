package capture

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"streamvault/core/organizer"
)

const linksPage = `<!doctype html><html><head><base href="https://cdn.example.com/dl/">
<link rel="next" href="?p=2">
</head><body>
<script>var x = "https://script.example.com/escondido.zip";</script>
<!-- <a href="comentado.pdf">c</a> -->
<a href="manual.pdf">Manual <b>do</b>
   utilizador</a>
<a href="/abs/pacote.zip#parte">Pacote</a>
<a href="https://outro.example.com/dados.csv?x=1&amp;y=2">Dados</a>
<a href="manual.pdf">repetido</a>
<a href="pagina.html">uma página</a>
<a href="sem-extensao">nada</a>
<a href="mailto:a@b.c">mail</a>
<a href="javascript:void(0)">js</a>
<a href="ftp://x.example.com/f.zip">ftp</a>
<a href="setup.exe"></a>
<a href="firmware.bin">fw</a>
<img src="foto.jpg" alt="Uma foto">
</body></html>`

func linkURLs(l []Link) []string {
	var o []string
	for _, x := range l {
		o = append(o, x.URL)
	}
	return o
}

func findLink(l []Link, u string) *Link {
	for i := range l {
		if l[i].URL == u {
			return &l[i]
		}
	}
	return nil
}

func TestExtractLinksFindsDownloadableFilesOnly(t *testing.T) {
	got, next := ExtractLinks("https://site.example.com/lista", []byte(linksPage), LinkOptions{})
	want := map[string]string{
		"https://cdn.example.com/dl/manual.pdf":       "Manual do utilizador", // texto limpo, sem tags nem quebras
		"https://cdn.example.com/abs/pacote.zip":      "Pacote",               // fragmento removido
		"https://outro.example.com/dados.csv?x=1&y=2": "Dados",                // &amp; desescapado
		"https://cdn.example.com/dl/setup.exe":        "setup.exe",            // sem texto: nome do ficheiro
	}
	for u, txt := range want {
		l := findLink(got, u)
		if l == nil {
			t.Errorf("faltou %s; obtidos: %v", u, linkURLs(got))
			continue
		}
		if l.Text != txt {
			t.Errorf("%s: texto %q, esperado %q", u, l.Text, txt)
		}
	}
	for _, bad := range []string{"comentado.pdf", "escondido.zip", "pagina.html", "sem-extensao", "mailto", "javascript", "ftp://", "foto.jpg", "firmware.bin"} {
		for _, u := range linkURLs(got) {
			if strings.Contains(u, bad) {
				t.Errorf("não devia incluir %q, mas veio %s", bad, u)
			}
		}
	}
	if n := len(got); n != len(want) {
		t.Errorf("esperava %d ligações (sem repetidas), vieram %d: %v", len(want), n, linkURLs(got))
	}
	if l := findLink(got, "https://cdn.example.com/dl/manual.pdf"); l == nil || l.Category != organizer.CatDocument || l.Ext != ".pdf" {
		t.Errorf("categoria/extensão do PDF: %+v", l)
	}
	if next != "https://cdn.example.com/dl/?p=2" {
		t.Errorf("página seguinte (link rel=next com <base>): %q", next)
	}
}

func TestExtractLinksFilters(t *testing.T) {
	src := "https://site.example.com/lista"
	only := func(lo LinkOptions) []string { l, _ := ExtractLinks(src, []byte(linksPage), lo); return linkURLs(l) }

	if got := only(LinkOptions{Categories: []string{"documentos"}}); len(got) != 2 { // pdf + csv, sem distinguir maiúsculas
		t.Errorf("categoria Documentos: %v", got)
	}
	if got := only(LinkOptions{Extensions: []string{"bin"}}); findLink(toLinks(got), "https://cdn.example.com/dl/firmware.bin") == nil {
		t.Errorf("extensão extra .bin devia ser aceite: %v", got)
	}
	if got := only(LinkOptions{Contains: "PACOTE"}); len(got) != 1 || !strings.HasSuffix(got[0], "pacote.zip") {
		t.Errorf("filtro de texto: %v", got)
	}
	// imagens só se pedidas explicitamente
	if got := only(LinkOptions{Categories: []string{organizer.CatImage}}); len(got) != 1 || !strings.HasSuffix(got[0], "foto.jpg") {
		t.Errorf("<img> só com a categoria Imagens: %v", got)
	}
}

func toLinks(us []string) []Link {
	var o []Link
	for _, u := range us {
		o = append(o, Link{URL: u})
	}
	return o
}

// Servidor paginado: p1 → p2 → p3; a p3 aponta para outro host e a p2 volta à p1.
type pagedSrv struct {
	*httptest.Server
	mu      sync.Mutex
	hits    []string
	cookies []string
}

func newPagedSrv(t *testing.T, requireCookie string) *pagedSrv {
	t.Helper()
	p := &pagedSrv{}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits = append(p.hits, "OUTRO"+r.URL.Path)
		p.cookies = append(p.cookies, "outro:"+r.Header.Get("Cookie"))
		p.mu.Unlock()
		fmt.Fprint(w, `<html><a href="nao-devia.zip">x</a></html>`)
	}))
	t.Cleanup(other.Close)
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits = append(p.hits, r.URL.Path)
		p.cookies = append(p.cookies, "main:"+r.Header.Get("Cookie"))
		p.mu.Unlock()
		if requireCookie != "" && r.Header.Get("Cookie") != requireCookie {
			http.Error(w, "login", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/p1":
			fmt.Fprint(w, `<a href="a1.pdf">a1</a><a rel="next" href="/p2">próxima</a>`)
		case "/p2":
			fmt.Fprint(w, `<a href="a2.pdf">a2</a><a href="a1.pdf">repetido</a><link rel="next" href="/p1">`) // ciclo
		case "/p3":
			fmt.Fprint(w, `<a href="a3.pdf">a3</a><a rel="next" href="`+other.URL+`/fora">fora</a>`)
		case "/go-other":
			http.Redirect(w, r, other.URL+"/redir", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func TestFindLinksFollowsNextPagesWithoutLoopsAndStaysOnHost(t *testing.T) {
	// p1 -> p2 -> (volta a p1: ciclo) ; p3 só é alcançada diretamente
	s := newPagedSrv(t, "")
	res, err := FindLinks(context.Background(), s.URL+"/p1", LinkOptions{
		Options: Options{AllowPrivateNetworks: true}, MaxPages: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 2 {
		t.Fatalf("p1 e p2, sem repetir a p1: %v", res.Pages)
	}
	if got := linkURLs(res.Links); len(got) != 2 { // a1 (1x) e a2
		t.Errorf("ligações únicas esperadas a1,a2: %v", got)
	}

	// próxima para OUTRO host: não se segue
	s2 := newPagedSrv(t, "")
	res, err = FindLinks(context.Background(), s2.URL+"/p3", LinkOptions{
		Options: Options{AllowPrivateNetworks: true}, MaxPages: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 1 || len(res.Links) != 1 {
		t.Errorf("a paginação nunca sai do host da primeira página: pages=%v links=%v", res.Pages, linkURLs(res.Links))
	}
	for _, h := range s2.hits {
		if strings.HasPrefix(h, "OUTRO") {
			t.Errorf("o outro host não devia ser contactado: %v", s2.hits)
		}
	}

	// MaxPages=1 (defeito): só a primeira
	s3 := newPagedSrv(t, "")
	res, _ = FindLinks(context.Background(), s3.URL+"/p1", LinkOptions{Options: Options{AllowPrivateNetworks: true}})
	if len(res.Pages) != 1 {
		t.Errorf("por defeito lê só a página pedida: %v", res.Pages)
	}
}

func TestFindLinksTruncatesAtLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, `<a href="f%d.zip">f</a>`, i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	res, err := FindLinks(context.Background(), srv.URL, LinkOptions{Options: Options{AllowPrivateNetworks: true}, MaxLinks: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Links) != 10 || !res.Truncated {
		t.Errorf("devia truncar em 10 e assinalar: n=%d truncated=%v", len(res.Links), res.Truncated)
	}
}

func TestFindLinksBlocksPrivateNetworksWhenNotAllowed(t *testing.T) {
	s := newPagedSrv(t, "")
	if _, err := FindLinks(context.Background(), s.URL+"/p1", LinkOptions{}); err == nil {
		t.Fatal("rede privada devia ser bloqueada quando AllowPrivateNetworks=false")
	}
	if len(s.hits) != 0 {
		t.Errorf("origem bloqueada não pode ser contactada: %v", s.hits)
	}
}

// Credenciais: enviadas à página, nunca a outro host (nem após redirecionamento).
func TestCookieIsSentToPageHostOnlyAndNotAfterCrossHostRedirect(t *testing.T) {
	const ck = "sid=segredo123"
	s := newPagedSrv(t, ck)

	// sem cookie: a página protegida recusa
	if _, err := FindLinks(context.Background(), s.URL+"/p1", LinkOptions{Options: Options{AllowPrivateNetworks: true}}); err == nil {
		t.Fatal("sem cookie devia falhar com 403")
	}
	// com cookie: funciona
	res, err := FindLinks(context.Background(), s.URL+"/p1", LinkOptions{Options: Options{AllowPrivateNetworks: true, Cookie: ck}})
	if err != nil || len(res.Links) != 1 {
		t.Fatalf("com cookie devia listar: err=%v res=%+v", err, res)
	}

	// redirecionamento para outro host: o Cookie não vai
	s.mu.Lock()
	s.hits, s.cookies = nil, nil
	s.mu.Unlock()
	_, _ = Find(context.Background(), s.URL+"/go-other", Options{AllowPrivateNetworks: true, Cookie: ck})
	s.mu.Lock()
	defer s.mu.Unlock()
	sawOther := false
	for _, c := range s.cookies {
		if strings.HasPrefix(c, "outro:") {
			sawOther = true
			if strings.Contains(c, "segredo123") {
				t.Fatalf("o cookie vazou para outro host: %q", c)
			}
		}
	}
	if !sawOther {
		t.Fatalf("o teste devia ter seguido o redirecionamento: %v", s.hits)
	}
}

func TestInvalidCookieIsNotSent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html></html>"))
	}))
	defer srv.Close()
	_, _ = Find(context.Background(), srv.URL, Options{AllowPrivateNetworks: true, Cookie: "a=b\r\nX-Injetado: 1"})
	if got != "" {
		t.Errorf("cookie com caracteres de controlo não pode ser enviado: %q", got)
	}
}
