// Package plugin implementa o contrato de adaptadores de origem (Fase 11,
// ADR-002 camada 3): adaptadores COMPILADOS no binário, que só atuam depois de
// aprovados explicitamente pelo utilizador, e cuja aprovação deixa de valer se
// o adaptador mudar (versão, esquemas ou restrições declaradas).
//
// O contrato é deliberadamente restrito: um adaptador só RECONHECE e NORMALIZA
// um link (sem rede, sem executar código descarregado). Não existe carregamento
// dinâmico de código de terceiros neste desenho.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"streamvault/core/domain"
	"streamvault/core/linkrec"
)

// Fingerprint identifica o COMPORTAMENTO DECLARADO de um adaptador. Qualquer
// alteração de nome, versão, esquemas ou restrições muda a impressão digital e
// invalida aprovações anteriores (o utilizador aprova de novo o que mudou).
func Fingerprint(a linkrec.Adapter) string {
	schemes := append([]string(nil), a.Schemes()...)
	sort.Strings(schemes)
	restr := append([]string(nil), a.Restrictions()...)
	sort.Strings(restr)
	b, _ := json.Marshal([]any{a.Name(), a.Version(), schemes, restr})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Approvals guarda quais adaptadores (e que impressão digital) o utilizador aprovou.
type Approvals interface {
	IsApproved(name, fingerprint string) bool
	// IsRevoked é verdadeiro se o utilizador revogou explicitamente o adaptador
	// (adaptadores do próprio produto só deixam de funcionar se isto acontecer).
	IsRevoked(name string) bool
	Approve(name, fingerprint string) error
	Revoke(name string) error
}

const revoked = "revoked"

// MemApprovals é uma implementação em memória (testes / sessão).
type MemApprovals struct {
	mu sync.Mutex
	m  map[string]string
}

func NewMemApprovals() *MemApprovals { return &MemApprovals{m: map[string]string{}} }

func (a *MemApprovals) IsApproved(name, fp string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	got, ok := a.m[name]
	return ok && got == fp
}
func (a *MemApprovals) Approve(name, fp string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[name] = fp
	return nil
}
func (a *MemApprovals) IsRevoked(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.m[name] == revoked
}
func (a *MemApprovals) Revoke(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[name] = revoked
	return nil
}

// FileApprovals persiste aprovações num ficheiro JSON (escrita atómica).
type FileApprovals struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

// OpenFileApprovals carrega (ou inicia vazio) o ficheiro de aprovações. Um
// ficheiro ilegível é um ERRO: nunca se assume "aprovado" por omissão.
func OpenFileApprovals(path string) (*FileApprovals, error) {
	f := &FileApprovals{path: path, m: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &f.m); err != nil {
		return nil, fmt.Errorf("plugin: aprovações ilegíveis em %s: %w", path, err)
	}
	return f, nil
}

func (f *FileApprovals) IsApproved(name, fp string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	got, ok := f.m[name]
	return ok && got == fp
}
func (f *FileApprovals) Approve(name, fp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[name] = fp
	return f.flush()
}
func (f *FileApprovals) IsRevoked(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[name] == revoked
}
func (f *FileApprovals) Revoke(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[name] = revoked
	return f.flush()
}
func (f *FileApprovals) flush() error {
	data, err := json.MarshalIndent(f.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// builtinSchemes não podem ser reclamados por plugins: pertencem ao núcleo.
var builtinSchemes = map[string]bool{"http": true, "https": true, "magnet": true, "file": true}

// Host gere os adaptadores compilados e aplica a política de aprovação.
type Host struct {
	mu        sync.RWMutex
	base      *linkrec.Registry // adaptadores do núcleo (http/magnet/torrent)
	approvals Approvals
	plugins   []linkrec.Adapter
	builtin   map[string]bool
}

// NewHost cria o host. base trata tudo o que nenhum plugin reconhece.
func NewHost(base *linkrec.Registry, approvals Approvals) *Host {
	return &Host{base: base, approvals: approvals, builtin: map[string]bool{}}
}

// RegisterBuiltin regista um adaptador COMPILADO PELO PRÓPRIO PRODUTO: funciona
// logo, sem pedir aprovação (o código vai no mesmo binário assinado). O
// utilizador pode revogá-lo; a revogação persiste. Adaptadores de terceiros
// continuam a usar Register e exigem aprovação explícita.
func (h *Host) RegisterBuiltin(a linkrec.Adapter) error {
	if err := h.Register(a); err != nil {
		return err
	}
	h.mu.Lock()
	h.builtin[a.Name()] = true
	h.mu.Unlock()
	return nil
}

// active diz se o adaptador pode atuar agora.
func (h *Host) active(a linkrec.Adapter) bool {
	if h.approvals.IsApproved(a.Name(), Fingerprint(a)) {
		return true
	}
	h.mu.RLock()
	b := h.builtin[a.Name()]
	h.mu.RUnlock()
	return b && !h.approvals.IsRevoked(a.Name())
}

// Register valida e regista um adaptador compilado. O adaptador fica INATIVO
// até o utilizador o aprovar (Approve).
func (h *Host) Register(a linkrec.Adapter) error {
	if strings.TrimSpace(a.Name()) == "" || strings.TrimSpace(a.Version()) == "" {
		return errors.New("plugin: nome e versão obrigatórios")
	}
	if len(a.Schemes()) == 0 {
		return fmt.Errorf("plugin %s: declara zero esquemas", a.Name())
	}
	if len(a.Restrictions()) == 0 {
		// ADR-002: um adaptador declara as suas limitações reais; sem isso não
		// há como informar honestamente o utilizador antes de aprovar.
		return fmt.Errorf("plugin %s: deve declarar Restrictions() (limitações reais)", a.Name())
	}
	for _, s := range a.Schemes() {
		if builtinSchemes[strings.ToLower(s)] {
			return fmt.Errorf("plugin %s: esquema %q pertence ao núcleo e não pode ser reclamado", a.Name(), s)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.plugins {
		if p.Name() == a.Name() {
			return fmt.Errorf("plugin: nome %q já registado", a.Name())
		}
	}
	h.plugins = append(h.plugins, a)
	return nil
}

// Info é o que a UI mostra ao utilizador antes de aprovar.
type Info struct {
	Name         string
	Version      string
	Schemes      []string
	Restrictions []string
	Fingerprint  string
	Approved     bool
	Builtin      bool // adaptador do próprio produto (ativo por defeito)
}

// List devolve todos os plugins registados e o estado de aprovação.
func (h *Host) List() []Info {
	h.mu.RLock()
	plugins := append([]linkrec.Adapter(nil), h.plugins...)
	h.mu.RUnlock()
	out := make([]Info, 0, len(plugins))
	for _, p := range plugins {
		h.mu.RLock()
		b := h.builtin[p.Name()]
		h.mu.RUnlock()
		out = append(out, Info{Name: p.Name(), Version: p.Version(), Schemes: p.Schemes(),
			Restrictions: p.Restrictions(), Fingerprint: Fingerprint(p), Approved: h.active(p), Builtin: b})
	}
	return out
}

func (h *Host) find(name string) linkrec.Adapter {
	for _, p := range h.plugins {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Approve regista a aprovação do utilizador para a versão ATUAL do adaptador.
func (h *Host) Approve(name string) error {
	h.mu.RLock()
	p := h.find(name)
	h.mu.RUnlock()
	if p == nil {
		return fmt.Errorf("plugin: %q não registado", name)
	}
	return h.approvals.Approve(name, Fingerprint(p))
}

// Revoke retira a aprovação.
func (h *Host) Revoke(name string) error { return h.approvals.Revoke(name) }

// Recognize tenta primeiro os plugins (mais específicos), depois o núcleo.
// Um plugin que RECONHECE o link mas não está aprovado bloqueia com explicação
// — nunca cai silenciosamente no adaptador HTTP genérico.
func (h *Host) Recognize(raw string) (src domain.DownloadSource, err error) {
	raw = strings.TrimSpace(raw)
	h.mu.RLock()
	plugins := append([]linkrec.Adapter(nil), h.plugins...)
	h.mu.RUnlock()
	for _, p := range plugins {
		if !safeCan(p, raw) {
			continue
		}
		if !h.active(p) {
			return domain.DownloadSource{}, domain.NewError(domain.ErrPolicyViolation, nil,
				fmt.Sprintf("o adaptador %q (v%s) reconhece este link mas ainda não foi aprovado.", p.Name(), p.Version()),
				"Reveja as limitações do adaptador e aprove-o nas definições para continuar.",
				domain.CertaintyObserved, false)
		}
		return parseGuarded(p, raw)
	}
	return h.base.Recognize(raw)
}

func safeCan(p linkrec.Adapter, raw string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return p.CanRecognize(raw)
}

// parseGuarded isola falhas do adaptador (panic) e valida o resultado: um
// adaptador só pode produzir origens que o núcleo sabe transferir.
func parseGuarded(p linkrec.Adapter, raw string) (src domain.DownloadSource, err error) {
	defer func() {
		if r := recover(); r != nil {
			src = domain.DownloadSource{}
			err = domain.NewError(domain.ErrUnknown, nil, fmt.Sprintf("o adaptador %q falhou internamente.", p.Name()),
				"Atualize ou desative o adaptador.", domain.CertaintyObserved, false)
		}
	}()
	s, perr := p.Parse(raw)
	if perr != nil {
		return domain.DownloadSource{}, perr
	}
	switch s.Kind {
	case domain.SourceHTTP, domain.SourceMagnet, domain.SourceTorrent:
	default:
		return domain.DownloadSource{}, domain.NewError(domain.ErrUnsupportedSource, nil,
			fmt.Sprintf("o adaptador %q devolveu um tipo de origem não suportado (%q).", p.Name(), s.Kind),
			"Atualize o adaptador.", domain.CertaintyObserved, false)
	}
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	s.Metadata["adapter"] = p.Name() + "@" + p.Version()
	return s, nil
}
