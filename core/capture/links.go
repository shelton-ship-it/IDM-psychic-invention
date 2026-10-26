package capture

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"streamvault/core/organizer"
)

// Limites do grabber (anti-abuso: memória, pedidos e tamanho da resposta).
const (
	MaxLinkPages = 10   // páginas seguidas por rel="next"
	MaxLinks     = 2000 // ligações devolvidas
	maxLinkText  = 200
)

// Link é uma ligação para um ficheiro descarregável encontrada numa página.
type Link struct {
	URL      string `json:"url"`
	Text     string `json:"text,omitempty"` // texto da ligação, ou nome do ficheiro
	Ext      string `json:"ext"`            // ".pdf", ".zip"…
	Category string `json:"category"`       // categorias do organizer (Documentos, Arquivos…)
	Page     string `json:"page"`           // página onde foi encontrada
}

// LinkOptions filtra o que o grabber devolve.
type LinkOptions struct {
	Options
	// Categories: vazio = todas as conhecidas exceto "Outros". Incluir
	// organizer.CatImage faz o grabber considerar também <img src> (ignorado por
	// defeito: inundaria a lista com ícones e miniaturas).
	Categories []string
	// Extensions: extensões extra aceites (ex.: ".bin"), além das categorias.
	Extensions []string
	// Contains: só URLs que contenham este texto (sem distinguir maiúsculas).
	Contains string
	// MaxPages: páginas a percorrer seguindo rel="next" (mínimo 1; máx MaxLinkPages).
	// Só se segue para o MESMO host da primeira página.
	MaxPages int
	// MaxLinks: limite de resultados (máx MaxLinks).
	MaxLinks int
}

// LinksResult é o resultado do grabber.
type LinksResult struct {
	Pages     []string `json:"pages"` // páginas realmente lidas (URL final)
	Links     []Link   `json:"links"`
	Truncated bool     `json:"truncated,omitempty"` // atingiu MaxLinks
}

var (
	anchorRe  = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a\s*>`)
	imgTagRe  = regexp.MustCompile(`(?is)<img\b([^>]*)>`)
	linkTagRe = regexp.MustCompile(`(?is)<link\b([^>]*)>`)
	stripTag  = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe   = regexp.MustCompile(`\s+`)
)

// ExtractLinks é a parte pura (sem rede): devolve as ligações descarregáveis de
// uma página, filtradas, sem repetições, e o URL da página seguinte (se houver).
func ExtractLinks(pageURL string, html []byte, lo LinkOptions) (links []Link, next string) {
	base, err := url.Parse(pageURL)
	if err != nil || base.Host == "" {
		return nil, ""
	}
	body := commentRe.ReplaceAllString(string(html), "")
	body = scriptRe.ReplaceAllString(body, "") // URLs dentro de scripts não são ligações da página
	for _, m := range tagRe.FindAllStringSubmatch(body, 200000) {
		if m[1] == "" && strings.EqualFold(m[2], "base") {
			if h := attrs(m[3])["href"]; h != "" {
				if nb, err := base.Parse(h); err == nil {
					base = nb
				}
			}
			break
		}
	}

	cats := map[string]bool{}
	for _, c := range lo.Categories {
		cats[strings.ToLower(strings.TrimSpace(c))] = true
	}
	wantImages := cats[strings.ToLower(organizer.CatImage)]
	exts := map[string]bool{}
	for _, e := range lo.Extensions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			exts[e] = true
		}
	}
	contains := strings.ToLower(lo.Contains)

	seen := map[string]bool{}
	add := func(raw, text string) {
		raw = strings.TrimSpace(htmlUnescape(raw))
		if raw == "" {
			return
		}
		u, err := base.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return
		}
		u.Fragment = ""
		full := u.String()
		if seen[full] {
			return
		}
		ext := strings.ToLower(path.Ext(u.Path))
		if ext == "" {
			return
		}
		cat := organizer.Categorize(path.Base(u.Path), "")
		switch {
		case exts[ext]:
		case cat == organizer.CatOther:
			return
		case len(cats) > 0 && !cats[strings.ToLower(cat)]:
			return
		}
		if contains != "" && !strings.Contains(strings.ToLower(full), contains) {
			return
		}
		seen[full] = true
		if text = cleanLinkText(text); text == "" {
			text = path.Base(u.Path)
		}
		links = append(links, Link{URL: full, Text: text, Ext: ext, Category: cat, Page: pageURL})
	}

	for _, m := range anchorRe.FindAllStringSubmatch(body, 200000) {
		a := attrs(m[1])
		if hasRelNext(a["rel"]) && next == "" {
			if u, err := base.Parse(strings.TrimSpace(htmlUnescape(a["href"]))); err == nil && u.Host != "" {
				u.Fragment = ""
				next = u.String()
			}
		}
		add(a["href"], m[2])
	}
	for _, m := range linkTagRe.FindAllStringSubmatch(body, 2000) {
		a := attrs(m[1])
		if next == "" && hasRelNext(a["rel"]) {
			if u, err := base.Parse(strings.TrimSpace(htmlUnescape(a["href"]))); err == nil && u.Host != "" {
				u.Fragment = ""
				next = u.String()
			}
		}
	}
	if wantImages {
		for _, m := range imgTagRe.FindAllStringSubmatch(body, 200000) {
			a := attrs(m[1])
			add(a["src"], a["alt"])
		}
	}
	return links, next
}

func hasRelNext(rel string) bool {
	for _, f := range strings.Fields(strings.ToLower(rel)) {
		if f == "next" {
			return true
		}
	}
	return false
}

func cleanLinkText(s string) string {
	s = htmlUnescape(stripTag.ReplaceAllString(s, " "))
	s = strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > maxLinkText {
		s = string(r[:maxLinkText]) + "…"
	}
	return s
}

// FindLinks descarrega a página (e, se pedido, as seguintes por rel="next") e
// devolve as ligações descarregáveis. Aplica a mesma política de segurança e de
// credenciais de Find; as páginas seguintes têm de estar no mesmo host.
func FindLinks(ctx context.Context, pageURL string, lo LinkOptions) (*LinksResult, error) {
	pages := lo.MaxPages
	if pages < 1 {
		pages = 1
	}
	if pages > MaxLinkPages {
		pages = MaxLinkPages
	}
	limit := lo.MaxLinks
	if limit < 1 || limit > MaxLinks {
		limit = MaxLinks
	}
	res := &LinksResult{}
	visited := map[string]bool{}
	var firstHost string
	cur := pageURL
	for i := 0; i < pages && cur != ""; i++ {
		if visited[cur] {
			break
		}
		visited[cur] = true
		resp, err := lo.Options.get(ctx, cur)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			break // uma página seguinte que falha não deita fora o que já foi lido
		}
		final, links, next, perr := readLinksPage(resp, lo)
		resp.Body.Close()
		if perr != nil {
			if i == 0 {
				return nil, perr
			}
			break
		}
		if i == 0 {
			if u, err := url.Parse(final); err == nil {
				firstHost = u.Host
			}
		}
		res.Pages = append(res.Pages, final)
		have := map[string]bool{}
		for _, l := range res.Links {
			have[l.URL] = true
		}
		for _, l := range links {
			if have[l.URL] {
				continue
			}
			if len(res.Links) >= limit {
				res.Truncated = true
				return res, nil
			}
			res.Links = append(res.Links, l)
		}
		cur = ""
		if next != "" {
			if nu, err := url.Parse(next); err == nil && strings.EqualFold(nu.Host, firstHost) {
				cur = next
			}
		}
	}
	return res, nil
}

func readLinksPage(resp *http.Response, lo LinkOptions) (final string, links []Link, next string, err error) {
	if resp.StatusCode != http.StatusOK {
		return "", nil, "", fmt.Errorf("a página respondeu HTTP %d", resp.StatusCode)
	}
	final = resp.Request.URL.String()
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "html") && ct != "" {
		return "", nil, "", fmt.Errorf("tipo de conteúdo não é uma página (%s)", strings.SplitN(ct, ";", 2)[0])
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxPageSize))
	if err != nil {
		return "", nil, "", err
	}
	links, next = ExtractLinks(final, body, lo)
	return final, links, next, nil
}
