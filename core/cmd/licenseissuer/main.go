// Comando licenseissuer: EMISSOR de licenças, isolado do cliente.
//
// Este binário é o único sítio que lida com a chave PRIVADA Ed25519. NÃO deve
// ser distribuído com as apps (Windows/Android): estas só embebem chaves
// públicas (license.Keyring). A chave privada nunca entra no repositório.
//
//	licenseissuer keygen -id key-2026-01 -out ./keys
//	licenseissuer issue  -key ./keys/key-2026-01.key -key-id key-2026-01 \
//	    -product streamvault-windows -edition pro -license-id lic-0001 \
//	    -days 0 -devices 3 -ad-free -torrent -out lic-0001.json
//	licenseissuer verify -pub ./keys/key-2026-01.pub -key-id key-2026-01 \
//	    -product streamvault-windows -in lic-0001.json
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"streamvault/core/license"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer, now func() time.Time) error {
	if len(args) == 0 {
		return errors.New("uso: licenseissuer <keygen|issue|verify> [opções]")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:], out)
	case "issue":
		return issue(args[1:], out, now)
	case "verify":
		return verify(args[1:], out, now)
	}
	return fmt.Errorf("comando desconhecido %q (keygen|issue|verify)", args[0])
}

func keygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	id := fs.String("id", "", "key_id (ex.: key-2026-01)")
	dir := fs.String("out", ".", "pasta de saída")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || strings.ContainsAny(*id, `/\ `) {
		return errors.New("keygen: -id obrigatório, sem espaços nem separadores")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	privPath, pubPath := filepath.Join(*dir, *id+".key"), filepath.Join(*dir, *id+".pub")
	for _, p := range []string{privPath, pubPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("keygen: %s já existe; recuse-se a sobrescrever uma chave", p)
		}
	}
	// 0600: só o dono lê a chave privada. O seed (32 bytes) basta para a recriar.
	if err := os.WriteFile(privPath, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(hex.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "chave privada: %s (GUARDAR OFFLINE; nunca distribuir)\nchave pública: %s (embeber nos clientes com key_id %q)\n", privPath, pubPath, *id)
	return nil
}

func readHexFile(path string, size int) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != size {
		return nil, fmt.Errorf("%s: esperado hex de %d bytes", path, size)
	}
	return raw, nil
}

func issue(args []string, out io.Writer, now func() time.Time) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	keyPath := fs.String("key", "", "ficheiro da chave privada (.key)")
	keyID := fs.String("key-id", "", "key_id a gravar na licença")
	product := fs.String("product", "", "product_id")
	edition := fs.String("edition", "pro", "edição")
	licID := fs.String("license-id", "", "license_id único")
	days := fs.Int("days", 0, "validade em dias (0 = permanente)")
	devices := fs.Int("devices", 1, "máximo de dispositivos")
	adFree := fs.Bool("ad-free", false, "direito: sem anúncios")
	prio := fs.Bool("priority-queue", false, "direito: fila prioritária")
	filters := fs.Bool("advanced-filters", false, "direito: filtros avançados")
	torrent := fs.Bool("torrent", false, "direito: motor torrent")
	lan := fs.Bool("lan", false, "direito: transferência LAN")
	dst := fs.String("out", "", "ficheiro de saída (.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for name, v := range map[string]string{"-key": *keyPath, "-key-id": *keyID, "-product": *product, "-license-id": *licID, "-out": *dst} {
		if v == "" {
			return fmt.Errorf("issue: %s obrigatório", name)
		}
	}
	if *days < 0 || *devices < 1 {
		return errors.New("issue: -days >= 0 e -devices >= 1")
	}
	seed, err := readHexFile(*keyPath, ed25519.SeedSize)
	if err != nil {
		return err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	t := now().UTC()
	l := &license.License{
		FormatVersion: license.FormatVersion, LicenseID: *licID, ProductID: *product, Edition: *edition,
		IssuedAt: t.Unix(),
		Entitlements: license.Entitlements{AdFree: *adFree, PriorityQueue: *prio,
			AdvancedFilters: *filters, TorrentEngine: *torrent, LANTransfer: *lan},
		ActivationPolicy: license.ActivationPolicy{MaxDevices: *devices},
		KeyID:            *keyID,
	}
	if *days > 0 {
		l.ExpiresAt = t.Add(time.Duration(*days) * 24 * time.Hour).Unix()
	}
	if err := l.Sign(priv); err != nil {
		return err
	}
	data, err := marshalLicense(l)
	if err != nil {
		return err
	}
	if _, err := os.Stat(*dst); err == nil {
		return fmt.Errorf("issue: %s já existe; recuse-se a sobrescrever", *dst)
	}
	if err := os.WriteFile(*dst, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "licença %s emitida (%s) → %s\n", *licID, *keyID, *dst)
	return nil
}

func verify(args []string, out io.Writer, now func() time.Time) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pubPath := fs.String("pub", "", "chave pública (.pub)")
	keyID := fs.String("key-id", "", "key_id da chave pública")
	product := fs.String("product", "", "product_id esperado")
	in := fs.String("in", "", "licença .json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pubPath == "" || *keyID == "" || *product == "" || *in == "" {
		return errors.New("verify: -pub, -key-id, -product e -in obrigatórios")
	}
	pub, err := readHexFile(*pubPath, ed25519.PublicKeySize)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	l, err := license.Parse(data)
	if err != nil {
		return err
	}
	ring := license.NewKeyring()
	if err := ring.Trust(*keyID, ed25519.PublicKey(pub)); err != nil {
		return err
	}
	res := (&license.Validator{ProductID: *product, Ring: ring, Now: now}).Validate(l)
	fmt.Fprintf(out, "estado: %s — %s\n", res.Status, res.Reason)
	if res.Status != license.StatusValid {
		return fmt.Errorf("licença não válida: %s", res.Status)
	}
	return nil
}

func marshalLicense(l *license.License) ([]byte, error) {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
