// Package capture encontra mídia descarregável numa página web, como o IDM:
// elementos <video>/<audio>/<source>, metatags og:video, ligações diretas a
// ficheiros de mídia, JSON-LD (VideoObject.contentUrl) e URLs de mídia dentro de
// scripts de leitores. Só devolve URLs que a própria página expõe.
//
// O que NÃO faz, por desenho (ADR-002/ADR-007/ADR-009): extratores específicos de
// sites, decifrar DRM nem contornar login/paywall. Streams HLS (.m3u8) são
// suportados pelo motor hlsengine (sem cifra, VOD), tal como DASH (.mpd) (ADR-011).
package capture

import (
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Kind classifica o candidato.
type Kind string

const (
	KindVideo  Kind = "video"
	KindAudio  Kind = "audio"
	KindStream Kind = "stream" // HLS/DASH: manifesto, não um ficheiro (HLS e DASH)
)

// Candidate é um URL de mídia encontrado na página.
type Candidate struct {
	URL       string `json:"url"`
	Kind      Kind   `json:"kind"`
	Title     string `json:"title,omitempty"`
	Found     string `json:"found"`     // onde foi encontrado: video, source, audio, og, link, json-ld, script
	Supported bool   `json:"supported"` // false se o formato não for descarregável
	Reason    string `json:"reason,omitempty"`
}

const maxCandidates = 100

var videoExt = map[string]bool{".mp4": true, ".webm": true, ".m4v": true, ".mov": true, ".mkv": true,
	".avi": true, ".flv": true, ".wmv": true, ".3gp": true, ".ogv": true}
var audioExt = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".oga": true,
	".opus": true, ".wav": true, ".flac": true}
var streamExt = map[string]bool{".m3u8": true, ".mpd": true}

// classify devolve o tipo a partir do caminho e/ou do atributo type="…".
func classify(rawURL, mime string) (Kind, bool) {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch {
	case mime == "application/vnd.apple.mpegurl" || mime == "application/x-mpegurl" || mime == "application/dash+xml":
		return KindStream, true
	case strings.HasPrefix(mime, "video/"):
		return KindVideo, true
	case strings.HasPrefix(mime, "audio/"):
		return KindAudio, true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch {
	case videoExt[ext]:
		return KindVideo, true
	case audioExt[ext]:
		return KindAudio, true
	case streamExt[ext]:
		return KindStream, true
	}
	return "", false
}

var (
	tagRe    = regexp.MustCompile(`(?is)<(/?)([a-z][a-z0-9:-]*)\b([^>]*)>`)
	attrRe   = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	scriptRe = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	titleRe  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	// URLs em texto livre (scripts de leitores): apanha o URL inteiro, incluindo
	// barras escapadas do JSON; quem decide se é mídia é classify(), pela extensão
	// exata do caminho (assim "video.mp4.html" não passa por mp4).
	mediaURLRe = regexp.MustCompile(`(?i)https?:(?:\\?/){2}(?:[^\s"'<>\\]|\\/|\\u0026|\\u003[dD])+`)
	commentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
)

func attrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		k := strings.ToLower(m[1])
		v := m[2] + m[3] + m[4]
		if _, ok := out[k]; !ok {
			out[k] = htmlUnescape(v)
		}
	}
	return out
}

func htmlUnescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return strings.NewReplacer("&amp;", "&", "&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&lt;", "<", "&gt;", ">").Replace(s)
}

func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(htmlUnescape(s)), " ")
	r := []rune(s)
	out := make([]rune, 0, len(r))
	for _, c := range r {
		if c >= 32 && c != 127 && len(out) < 200 {
			out = append(out, c)
		}
	}
	return string(out)
}

type extractor struct {
	base  *url.URL
	title string
	seen  map[string]bool
	out   []Candidate
}

func (e *extractor) add(raw, mime, found string) {
	if len(e.out) >= maxCandidates {
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || strings.HasPrefix(raw, "blob:") || strings.HasPrefix(raw, "data:") ||
		strings.HasPrefix(raw, "javascript:") {
		return
	}
	u, err := e.base.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return
	}
	u.Fragment = ""
	kind, ok := classify(u.String(), mime)
	if !ok {
		return
	}
	full := u.String()
	if e.seen[full] {
		return
	}
	e.seen[full] = true
	// HLS (.m3u8) e DASH (.mpd) são ambos descarregados pelo núcleo; cifra/DRM, ao vivo
	// e formatos não suportados são recusados LÁ, com explicação (a captura não os vê).
	c := Candidate{URL: full, Kind: kind, Title: e.title, Found: found, Supported: true}
	e.out = append(e.out, c)
}

// Extract devolve a mídia encontrada no HTML de pageURL (URLs já resolvidos e
// deduplicados, no máximo 100).
func Extract(pageURL string, html []byte) []Candidate {
	base, err := url.Parse(pageURL)
	if err != nil || base.Host == "" {
		return nil
	}
	src := string(html)
	e := &extractor{base: base, seen: map[string]bool{}}
	// <base href> altera a resolução de URLs relativos
	body := commentRe.ReplaceAllString(src, "")
	for _, m := range tagRe.FindAllStringSubmatch(body, 200000) {
		if m[1] == "" && strings.EqualFold(m[2], "base") {
			if h := attrs(m[3])["href"]; h != "" {
				if nb, err := base.Parse(h); err == nil {
					e.base = nb
				}
			}
			break
		}
	}
	if m := titleRe.FindStringSubmatch(body); m != nil {
		e.title = cleanTitle(m[1])
	}

	// scripts: JSON-LD e URLs de mídia em texto
	for _, m := range scriptRe.FindAllStringSubmatch(body, 2000) {
		a := attrs(m[1])
		if strings.Contains(strings.ToLower(a["type"]), "ld+json") {
			e.jsonLD(m[2])
			continue
		}
		for _, u := range mediaURLRe.FindAllString(m[2], 3000) {
			e.add(unescapeJS(u), "", "script")
		}
	}
	// marcação (sem o conteúdo dos scripts)
	noScripts := scriptRe.ReplaceAllString(body, "")
	var parent string // <video>/<audio> aberto
	for _, m := range tagRe.FindAllStringSubmatch(noScripts, 200000) {
		closing, name, rest := m[1] == "/", strings.ToLower(m[2]), m[3]
		if closing {
			if name == "video" || name == "audio" {
				parent = ""
			}
			continue
		}
		a := attrs(rest)
		switch name {
		case "video", "audio":
			parent = name
			e.add(a["src"], a["type"], name)
			if name == "video" {
				e.add(a["data-src"], "", "video")
			}
		case "source":
			if parent != "" {
				e.add(a["src"], a["type"], "source")
			}
		case "meta":
			p := strings.ToLower(a["property"] + a["name"])
			switch p {
			case "og:video", "og:video:url", "og:video:secure_url", "og:audio", "og:audio:url",
				"og:audio:secure_url", "twitter:player:stream":
				e.add(a["content"], a["itemprop"], "og")
			}
		case "a":
			e.add(a["href"], "", "link")
		case "link":
			if strings.EqualFold(a["rel"], "preload") && (a["as"] == "video" || a["as"] == "audio") {
				e.add(a["href"], "", "link")
			}
		}
	}
	return e.out
}

func unescapeJS(s string) string {
	return strings.NewReplacer(`\/`, "/", `\u0026`, "&", `\u003d`, "=", `\u003D`, "=", `&amp;`, "&").Replace(s)
}

// jsonLD lê VideoObject/AudioObject.contentUrl (NÃO embedUrl: é uma página de leitor).
func (e *extractor) jsonLD(raw string) {
	var v any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &v) != nil {
		return
	}
	var walk func(any, int)
	walk = func(x any, depth int) {
		if depth > 8 {
			return
		}
		switch t := x.(type) {
		case []any:
			for _, i := range t {
				walk(i, depth+1)
			}
		case map[string]any:
			if s, ok := t["contentUrl"].(string); ok {
				e.add(s, "", "json-ld")
			}
			for _, k := range []string{"@graph", "video", "audio", "mainEntity", "hasPart"} {
				if c, ok := t[k]; ok {
					walk(c, depth+1)
				}
			}
		}
	}
	walk(v, 0)
}
