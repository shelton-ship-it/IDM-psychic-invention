package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

func TestKeygenIssueVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := run([]string{"keygen", "-id", "key-t", "-out", dir}, &out, fixedNow); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "key-t.key"))
	if err != nil {
		t.Fatalf("chave privada em falta: %v", err)
	}
	// o Windows não tem permissões POSIX (Perm() devolve sempre 0666)
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("chave privada deve ter permissão 0600: %v", fi.Mode().Perm())
	}
	lic := filepath.Join(dir, "lic.json")
	if err := run([]string{"issue", "-key", filepath.Join(dir, "key-t.key"), "-key-id", "key-t",
		"-product", "streamvault-windows", "-license-id", "lic-1", "-days", "30", "-devices", "2",
		"-ad-free", "-torrent", "-out", lic}, &out, fixedNow); err != nil {
		t.Fatal(err)
	}
	ok := []string{"verify", "-pub", filepath.Join(dir, "key-t.pub"), "-key-id", "key-t",
		"-product", "streamvault-windows", "-in", lic}
	if err := run(ok, &out, fixedNow); err != nil {
		t.Fatalf("licença emitida deve validar: %v", err)
	}
	// produto errado
	bad := append([]string{}, ok...)
	bad[6] = "outro-produto"
	if err := run(bad, &out, fixedNow); err == nil {
		t.Error("produto diferente deveria falhar")
	}
	// expirada (relógio 60 dias à frente)
	late := func() time.Time { return fixedNow().Add(60 * 24 * time.Hour) }
	if err := run(ok, &out, late); err == nil {
		t.Error("licença expirada deveria falhar")
	}
	// adulteração: ativar um direito sem reassinar
	data, _ := os.ReadFile(lic)
	tampered := strings.Replace(string(data), `"lan_transfer": false`, `"lan_transfer": true`, 1)
	if tampered == string(data) {
		t.Fatal("teste inválido: campo não encontrado")
	}
	os.WriteFile(lic, []byte(tampered), 0o644)
	if err := run(ok, &out, fixedNow); err == nil {
		t.Error("licença adulterada deveria falhar")
	}
}

func TestRefusesToOverwriteKeysAndLicenses(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	args := []string{"keygen", "-id", "k", "-out", dir}
	if err := run(args, &out, fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &out, fixedNow); err == nil {
		t.Error("keygen não pode sobrescrever chave existente")
	}
}

func TestIssueRequiresAllFields(t *testing.T) {
	if err := run([]string{"issue", "-key", "x"}, &bytes.Buffer{}, fixedNow); err == nil {
		t.Error("campos em falta deveriam ser recusados")
	}
	if err := run(nil, &bytes.Buffer{}, fixedNow); err == nil {
		t.Error("sem comando deveria falhar")
	}
}
