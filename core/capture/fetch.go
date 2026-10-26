package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"streamvault/core/security"
)

// MaxPageSize limita o HTML lido (anti-abuso de memória).
const MaxPageSize = 2 << 20

// Options da captura.
type Options struct {
	// AllowPrivateNetworks: a página foi escolhida pelo utilizador, por isso
	// redes privadas são permitidas se a configuração o permitir (ADR-006).
	AllowPrivateNetworks bool
	Client               *http.Client
	UserAgent            string
	Referer              string
	// Cookie: cabeçalho Cookie da sessão do utilizador (páginas atrás de login).
	// Só é enviado ao host EXATO da página pedida e é retirado em redirecionamentos
	// para outro host ou de https para http (ADR-010). Nunca é registado.
	Cookie string
}

// Result é o resultado de uma captura.
type Result struct {
	PageURL    string      `json:"page_url"` // URL final (após redirecionamentos)
	Title      string      `json:"title,omitempty"`
	Candidates []Candidate `json:"candidates"`
}

// Find descarrega a página e devolve a mídia encontrada. Se o URL já for um
// ficheiro de mídia (Content-Type video/* ou audio/*), devolve-o como único candidato.
func Find(ctx context.Context, pageURL string, o Options) (*Result, error) {
	resp, err := o.get(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a página respondeu HTTP %d", resp.StatusCode)
	}
	final := resp.Request.URL.String()
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") {
		kind, _ := classify(final, ct)
		return &Result{PageURL: final, Candidates: []Candidate{{URL: final, Kind: kind, Found: "direct", Supported: true}}}, nil
	}
	if !strings.Contains(ct, "html") && ct != "" {
		return nil, fmt.Errorf("tipo de conteúdo não é uma página (%s)", strings.SplitN(ct, ";", 2)[0])
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxPageSize))
	if err != nil {
		return nil, err
	}
	cands := Extract(final, body)
	r := &Result{PageURL: final, Candidates: cands}
	if len(cands) > 0 {
		r.Title = cands[0].Title
	} else if m := titleRe.FindStringSubmatch(string(body)); m != nil {
		r.Title = cleanTitle(m[1])
	}
	return r, nil
}

// get faz o pedido GET de uma página com a política do produto: bloqueio por
// segurança, redirecionamentos que não saem para a rede interna, e credenciais
// confinadas ao host da página. O chamador fecha resp.Body.
func (o Options) get(ctx context.Context, pageURL string) (*http.Response, error) {
	sopts := security.DefaultOptions()
	sopts.AllowPrivateNetworks = o.AllowPrivateNetworks
	for _, f := range security.CheckURL(pageURL, sopts) {
		if f.Severity == "block" {
			return nil, fmt.Errorf("página bloqueada: %s", f.Message)
		}
	}
	hc := o.Client
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	// política de redirecionamento: origem pública não pode ir para rede privada
	cl := *hc
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return errors.New("demasiados redirecionamentos")
		}
		if len(via) > 0 && req.Header.Get("Cookie") != "" {
			first := via[0].URL
			if !strings.EqualFold(req.URL.Host, first.Host) || (first.Scheme == "https" && req.URL.Scheme != "https") {
				req.Header.Del("Cookie")
			}
		}
		opts := security.DefaultOptions()
		if len(via) > 0 && security.IsPrivateOrigin(via[0].URL.String()) {
			opts.AllowPrivateNetworks = true
		}
		for _, f := range security.CheckURL(req.URL.String(), opts) {
			if f.Severity == "block" {
				return fmt.Errorf("redirecionamento bloqueado: %s", f.Message)
			}
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "StreamVault/1 (captura de página)"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	if o.Referer != "" {
		req.Header.Set("Referer", o.Referer)
	}
	if ck := o.Cookie; ck != "" && validCookieHeader(ck) {
		req.Header.Set("Cookie", ck)
	}
	return cl.Do(req)
}

// validCookieHeader: sem vazios, sem excesso e sem caracteres de controlo.
func validCookieHeader(v string) bool {
	if v == "" || len(v) > 8192 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 32 || v[i] > 126 {
			return false
		}
	}
	return true
}
