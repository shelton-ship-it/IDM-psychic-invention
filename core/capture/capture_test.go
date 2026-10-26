package capture

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func urls(c []Candidate) []string {
	var out []string
	for _, x := range c {
		out = append(out, x.URL)
	}
	return out
}

func find(c []Candidate, u string) *Candidate {
	for i := range c {
		if c[i].URL == u {
			return &c[i]
		}
	}
	return nil
}

const page = `<!doctype html><html><head>
<title>  Aula   1 &amp; 2
</title>
<base href="https://cdn.example.com/base/">
<meta property="og:video:secure_url" content="https://og.example.com/og.mp4">
<meta property="og:video" content="/og-relativo.webm">
<link rel="preload" as="video" href="pre.mp4">
<script type="application/ld+json">{"@context":"https://schema.org","@graph":[
 {"@type":"VideoObject","contentUrl":"https://ld.example.com/ld.mp4","embedUrl":"https://ld.example.com/player/123"}]}</script>
<script>var cfg={"file":"https:\/\/js.example.com\/a\/clip.mp4?sig=ab\u0026e=9","hls":"https:\/\/js.example.com\/live.m3u8","dash":"https:\/\/js.example.com\/manifest.mpd","x":"https://js.example.com/page.html","y":"https://js.example.com/video.mp4.html"};</script>
<!-- <video src="https://comentado.example.com/c.mp4"></video> -->
</head><body>
<video src="v1.mp4" poster="p.jpg"></video>
<video controls><source src="v2.webm" type="video/webm"><source src="v2.mp4" type="video/mp4"></video>
<audio src="/abs/som.mp3"></audio>
<picture><source srcset="img.webp" type="image/webp"><source src="ignorado.mp4"></picture>
<a href="download/aula.mkv#t=10">aula</a> <a href="https://x.example.com/doc.pdf">pdf</a>
<a href="blob:https://x/abc">blob</a> <a href="javascript:void(0)">js</a> <a href="data:video/mp4;base64,AAAA">d</a>
<video src="v1.mp4"></video>
<a href="ftp://x.example.com/f.mp4">ftp</a>
</body></html>`

func TestExtractFindsMediaWhereIDMWould(t *testing.T) {
	got := Extract("https://site.example.com/aulas/1", []byte(page))
	want := []string{
		"https://js.example.com/a/clip.mp4?sig=ab&e=9", // script, com \/ e \u0026 desescapados
		"https://js.example.com/live.m3u8",
		"https://ld.example.com/ld.mp4", // contentUrl
		"https://cdn.example.com/base/v1.mp4",
		"https://cdn.example.com/base/v2.webm",
		"https://cdn.example.com/base/v2.mp4",
		"https://cdn.example.com/abs/som.mp3",
		"https://og.example.com/og.mp4",
		"https://cdn.example.com/og-relativo.webm",
		"https://cdn.example.com/base/pre.mp4",
		"https://cdn.example.com/base/download/aula.mkv", // fragmento removido
	}
	have := map[string]bool{}
	for _, u := range urls(got) {
		have[u] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("faltou: %s\n  encontrados: %v", w, urls(got))
		}
	}
	for _, bad := range []string{
		"https://ld.example.com/player/123",         // embedUrl não é mídia
		"https://comentado.example.com/c.mp4",       // comentário HTML
		"https://js.example.com/page.html",          // não é mídia
		"https://js.example.com/video.mp4.html",     // extensão final é .html
		"https://cdn.example.com/base/ignorado.mp4", // <source> fora de video/audio
		"https://x.example.com/doc.pdf",
	} {
		if have[bad] {
			t.Errorf("não devia ser candidato: %s", bad)
		}
	}
	for u := range have {
		if strings.HasPrefix(u, "blob:") || strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "ftp:") || strings.HasPrefix(u, "javascript:") {
			t.Errorf("esquema não descarregável: %s", u)
		}
	}
	// dedupe: v1.mp4 aparece duas vezes na página
	n := 0
	for _, u := range urls(got) {
		if u == "https://cdn.example.com/base/v1.mp4" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("v1.mp4 deveria aparecer 1x, apareceu %d", n)
	}
}

func TestHLSAndDASHAreSupportedAndTitleIsClean(t *testing.T) {
	got := Extract("https://site.example.com/aulas/1", []byte(page))
	hls := find(got, "https://js.example.com/live.m3u8")
	if hls == nil || hls.Kind != KindStream || !hls.Supported || hls.Reason != "" {
		t.Fatalf("HLS deve vir como suportado e sem motivo de recusa: %+v", hls)
	}
	dash := find(got, "https://js.example.com/manifest.mpd")
	if dash == nil || dash.Kind != KindStream || !dash.Supported || dash.Reason != "" {
		t.Fatalf("DASH deve vir como suportado e sem motivo de recusa: %+v", dash)
	}
	mp4 := find(got, "https://cdn.example.com/base/v1.mp4")
	if mp4 == nil || !mp4.Supported || mp4.Kind != KindVideo || mp4.Found != "video" {
		t.Fatalf("mp4: %+v", mp4)
	}
	if mp4.Title != "Aula 1 & 2" {
		t.Errorf("título limpo: %q", mp4.Title)
	}
	if a := find(got, "https://cdn.example.com/abs/som.mp3"); a == nil || a.Kind != KindAudio {
		t.Errorf("mp3 deve ser áudio: %+v", a)
	}
}

func TestExtractIsBoundedAndSafeOnHostileInput(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, `<video src="https://x.example.com/v%d.mp4"></video>`, i)
	}
	if got := Extract("https://s.example.com/", []byte(b.String())); len(got) != maxCandidates {
		t.Errorf("deve limitar a %d candidatos, devolveu %d", maxCandidates, len(got))
	}
	for _, h := range []string{"", "<", "<video src=", `<video src="https://x/a.mp4`, strings.Repeat("<a href=", 5000),
		"<script>" + strings.Repeat("https://x.example.com/", 10000) + "</script>"} {
		Extract("https://s.example.com/", []byte(h)) // não pode entrar em pânico nem demorar
	}
	if Extract("não-é-url", []byte(page)) != nil {
		t.Error("URL base inválida deve devolver nil")
	}
	if got := Extract("https://s.example.com/", []byte(`<script type="application/ld+json">{lixo</script>`)); len(got) != 0 {
		t.Errorf("JSON-LD inválido não deve produzir candidatos: %v", got)
	}
}

func TestFindFetchesPageAndPassesHeaders(t *testing.T) {
	var mu sync.Mutex
	var ref, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ref, ua = r.Header.Get("Referer"), r.Header.Get("User-Agent")
		mu.Unlock()
		switch r.URL.Path {
		case "/pagina":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<title>T</title><video src="clip.mp4"></video>`))
		case "/direto.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("x"))
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{}"))
		case "/grande":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(strings.Repeat("a", MaxPageSize+5000) + `<video src="tarde.mp4"></video>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	r, err := Find(context.Background(), srv.URL+"/pagina", Options{AllowPrivateNetworks: true, UserAgent: "Navegador/1", Referer: "https://origem.example.com/"})
	if err != nil || len(r.Candidates) != 1 || r.Candidates[0].URL != srv.URL+"/clip.mp4" || r.Title != "T" {
		t.Fatalf("%+v %v", r, err)
	}
	mu.Lock()
	if ref != "https://origem.example.com/" || ua != "Navegador/1" {
		t.Errorf("cabeçalhos não propagados: referer=%q ua=%q", ref, ua)
	}
	mu.Unlock()
	d, err := Find(context.Background(), srv.URL+"/direto.mp4", Options{AllowPrivateNetworks: true})
	if err != nil || len(d.Candidates) != 1 || d.Candidates[0].Found != "direct" || d.Candidates[0].Kind != KindVideo {
		t.Fatalf("ligação direta a mídia: %+v %v", d, err)
	}
	if _, err := Find(context.Background(), srv.URL+"/api", Options{AllowPrivateNetworks: true}); err == nil {
		t.Error("JSON não é uma página")
	}
	if _, err := Find(context.Background(), srv.URL+"/nada", Options{AllowPrivateNetworks: true}); err == nil {
		t.Error("HTTP 404 deve falhar")
	}
	g, err := Find(context.Background(), srv.URL+"/grande", Options{AllowPrivateNetworks: true})
	if err != nil || len(g.Candidates) != 0 {
		t.Errorf("HTML acima do limite é truncado (o vídeo depois do limite não conta): %+v %v", g, err)
	}
	// modo estrito: página em loopback bloqueada
	if _, err := Find(context.Background(), srv.URL+"/pagina", Options{AllowPrivateNetworks: false}); err == nil {
		t.Error("rede privada deve ser bloqueada em modo estrito")
	}
}
