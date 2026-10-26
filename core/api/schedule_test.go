package api

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streamvault/core/config"
	"streamvault/core/scheduler"
)

func startSched(t *testing.T, e *env) {
	t.Helper()
	if err := e.core.StartScheduler(filepath.Join(t.TempDir(), "schedule.json"), scheduler.Options{Interval: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

func inMs(ms int) string {
	return time.Now().Add(time.Duration(ms) * time.Millisecond).UTC().Format(time.RFC3339Nano)
}

func TestScheduleStartsTheDownloadAtTheChosenTimeAndRedactsTheView(t *testing.T) {
	e := newEnv(t, nil)
	startSched(t, e)
	srv, _ := protectedServer(t, 30_000, true)

	code, m, raw := e.do("POST", "/v1/schedule", map[string]any{
		"url": srv.URL + "/noite.bin?token=SEGREDO-AGENDA", "at": inMs(700), "cookie": apiCookie}, tok, nil)
	if code != 201 {
		t.Fatalf("agendar: %d %s", code, raw)
	}
	id := m["id"].(string)
	if m["status"] != "pending" || m["needs_cookie"] != true {
		t.Errorf("entrada: %v", m)
	}
	if strings.Contains(string(raw), "SEGREDO-AGENDA") || strings.Contains(string(raw), "segredo-api-123") {
		t.Fatalf("a resposta expôs a query do URL ou o cookie: %s", raw)
	}
	// antes da hora: está na agenda e ainda não há tarefa
	_, _, list := e.do("GET", "/v1/schedule", nil, tok, nil)
	if !strings.Contains(string(list), id) || strings.Contains(string(list), "SEGREDO") {
		t.Fatalf("a agenda devia listar a entrada, sem segredos: %s", list)
	}
	if n := len(e.tasks()); n != 0 {
		t.Fatalf("não devia haver tarefas antes da hora: %d", n)
	}
	// à hora: passa a tarefa da fila (com o cookie, que está em memória) e conclui
	deadline := time.Now().Add(10 * time.Second)
	for len(e.tasks()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("o download agendado nunca arrancou")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.waitState(e.tasks()[0]["id"].(string), "completed")
	_, _, list = e.do("GET", "/v1/schedule", nil, tok, nil)
	if strings.TrimSpace(string(list)) != "[]" {
		t.Errorf("depois de arrancar a entrada sai da agenda: %s", list)
	}
}

func TestScheduleValidationDeleteAndAuth(t *testing.T) {
	e := newEnv(t, nil)
	// sem agendador ativo
	if code, _, _ := e.do("GET", "/v1/schedule", nil, tok, nil); code != 503 {
		t.Errorf("sem agendador esperava 503, obtido %d", code)
	}
	startSched(t, e)
	srv, _ := protectedServer(t, 1000, false)

	if code, _, _ := e.do("GET", "/v1/schedule", nil, "", nil); code != 401 {
		t.Errorf("sem token: %d", code)
	}
	bad := map[string]map[string]any{
		"hora em falta":      {"url": srv.URL + "/f.bin"},
		"hora inválida":      {"url": srv.URL + "/f.bin", "at": "amanhã às 2"},
		"url vazia":          {"url": "", "at": inMs(60000)},
		"muito longe":        {"url": srv.URL + "/f.bin", "at": time.Now().AddDate(2, 0, 0).UTC().Format(time.RFC3339)},
		"cookie inválido":    {"url": srv.URL + "/f.bin", "at": inMs(60000), "cookie": "a=b\r\nEvil: 1"},
		"campo desconhecido": {"url": srv.URL + "/f.bin", "at": inMs(60000), "x": 1},
	}
	for name, body := range bad {
		if code, _, raw := e.do("POST", "/v1/schedule", body, tok, nil); code < 400 || code >= 500 {
			t.Errorf("%s: esperava 4xx, obtido %d %s", name, code, raw)
		}
	}
	if _, _, l := e.do("GET", "/v1/schedule", nil, tok, nil); strings.TrimSpace(string(l)) != "[]" {
		t.Errorf("pedidos inválidos não agendam nada: %s", l)
	}

	code, m, raw := e.do("POST", "/v1/schedule", map[string]any{"url": srv.URL + "/f.bin", "at": inMs(3600_000)}, tok, nil)
	if code != 201 {
		t.Fatalf("%d %s", code, raw)
	}
	id := m["id"].(string)
	if code, _, _ := e.do("DELETE", "/v1/schedule/"+id, nil, tok, nil); code != 204 {
		t.Errorf("DELETE: %d", code)
	}
	if code, _, _ := e.do("DELETE", "/v1/schedule/"+id, nil, tok, nil); code != 404 {
		t.Errorf("DELETE repetido: %d (esperado 404)", code)
	}
}

// A política de segurança aplica-se ao agendar, não só à hora marcada.
func TestScheduleBlocksForbiddenSourcesImmediately(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Security.AllowPrivateNetworks = false })
	startSched(t, e)
	srv, _ := protectedServer(t, 1000, false) // loopback
	code, _, raw := e.do("POST", "/v1/schedule", map[string]any{"url": srv.URL + "/f.bin", "at": inMs(60000)}, tok, nil)
	if code != 403 {
		t.Fatalf("rede privada em modo estrito: esperava 403, obtido %d %s", code, raw)
	}
	if _, _, l := e.do("GET", "/v1/schedule", nil, tok, nil); strings.TrimSpace(string(l)) != "[]" {
		t.Errorf("nada devia ficar agendado: %s", l)
	}
}
