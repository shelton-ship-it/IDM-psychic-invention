package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"

	"streamvault/core/domain"
	"streamvault/core/engine"
	"streamvault/core/httpengine"
)

// secretStore guarda credenciais (cabeçalho Cookie) SÓ em memória. É deliberado
// (ADR-010): o que fica no SQLite/ficheiros é apenas um token opaco; reiniciar
// o programa apaga as credenciais e a tarefa falha com explicação, em vez de
// as deixar em claro no disco ou de depender de um cofre específico de cada SO.
type secretStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newSecretStore() *secretStore { return &secretStore{m: map[string]string{}} }

// put guarda o valor e devolve o token opaco que o referencia.
func (s *secretStore) put(v string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("app: sem entropia: " + err.Error())
	}
	ref := hex.EncodeToString(b[:])
	s.mu.Lock()
	s.m[ref] = v
	s.mu.Unlock()
	return ref
}

func (s *secretStore) get(ref string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ref]
	return v, ok
}

func (s *secretStore) drop(ref string) {
	s.mu.Lock()
	delete(s.m, ref)
	s.mu.Unlock()
}

// wipe apaga tudo (shutdown).
func (s *secretStore) wipe() {
	s.mu.Lock()
	s.m = map[string]string{}
	s.mu.Unlock()
}

// validCookie recusa cabeçalhos vazios, demasiado longos ou com caracteres de
// controlo (injeção de cabeçalhos).
func validCookie(v string) bool {
	if v == "" || len(v) > httpengine.MaxCookieHeader || strings.TrimSpace(v) != v {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 32 || v[i] > 126 {
			return false
		}
	}
	return true
}

// cookieEngine decora um motor: injeta o cookie (numa CÓPIA da tarefa, que só
// existe em memória) antes de o motor arrancar. Se o token existe mas o valor
// já não (o programa reiniciou), falha com explicação — nunca tenta sem login
// para depois gravar uma página de erro como se fosse o ficheiro.
type cookieEngine struct {
	engine.Engine
	secrets *secretStore
}

func (w cookieEngine) Start(ctx context.Context, req engine.StartRequest) (engine.JobHandle, error) {
	ref := req.Task.Source.Metadata["cookie_ref"]
	if ref == "" {
		return w.Engine.Start(ctx, req)
	}
	v, ok := w.secrets.get(ref)
	if !ok {
		return nil, domain.NewError(domain.ErrAuthRequired, nil,
			"As credenciais (cookies) desta transferência só existem em memória e perderam-se.",
			"Inicie sessão no site e volte a adicionar a transferência a partir do navegador.",
			domain.CertaintyObserved, false)
	}
	md := make(map[string]string, len(req.Task.Source.Metadata)+1)
	for k, val := range req.Task.Source.Metadata {
		md[k] = val
	}
	md["cookie"] = v
	req.Task.Source.Metadata = md
	return w.Engine.Start(ctx, req)
}
