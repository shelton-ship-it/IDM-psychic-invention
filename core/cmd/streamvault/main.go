// Comando streamvault: daemon local do StreamVault (Windows e outros SOs).
// Abre o núcleo, serve a API + UI em 127.0.0.1 e imprime o endereço de acesso
// (a chave vai no fragmento #token=..., que o navegador nunca envia ao servidor).
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"streamvault/core/api"
	"streamvault/core/app"
	"streamvault/core/btnative"
	"streamvault/core/config"
	"streamvault/core/domain"
	"streamvault/core/lan"
	"streamvault/core/license"
	"streamvault/core/scheduler"
	"streamvault/core/version"
)

const defaultAddr = "127.0.0.1:47800" // porta estável: a extensão do navegador liga-se aqui

type opener func(cfg config.Config, dbPath string, opts ...app.Option) (*app.Core, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, app.Open); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer, open opener) error {
	fs := flag.NewFlagSet("streamvault", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "endereço de escuta (só loopback); se a porta por defeito estiver ocupada usa uma livre")
	dataDir := fs.String("data", "", "pasta de dados (base SQLite, aprovações); defeito: pasta de configuração do utilizador")
	dlDir := fs.String("dir", "", "pasta de destino dos downloads; defeito: ~/Downloads/StreamVault")
	cfgPath := fs.String("config", "", "ficheiro JSON de configuração (opcional)")
	openBrowser := fs.Bool("open", false, "abrir o navegador por defeito")
	showVersion := fs.Bool("version", false, "mostrar a versão")
	newToken := fs.Bool("new-token", false, "gerar uma nova chave de acesso (invalida a extensão do navegador até ser reconfigurada)")
	licPath := fs.String("license", "", "ficheiro da licença (.json)")
	licPub := fs.String("license-pub", "", "chave pública do emissor (.pub, hex)")
	licKeyID := fs.String("license-key-id", "", "key_id da chave pública")
	lanListen := fs.String("lan-listen", "", "ativa o recetor LAN neste endereço (ex.: :47821); exige licença com LAN")
	lanAnnounce := fs.Bool("lan-announce", false, "anunciar este dispositivo na rede (broadcast UDP)")
	noTorrent := fs.Bool("no-torrent", false, "desativa magnet/.torrent")
	noDHT := fs.Bool("no-dht", false, "não usar a DHT para encontrar peers")
	btListen := fs.String("bt-listen", ":6881", "endereço onde escutar peers BitTorrent (se ocupado, usa uma porta livre)")
	btLAN := fs.Bool("bt-lan-peers", false, "permitir peers/trackers em redes privadas (úteis para swarms locais)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintf(out, "streamvault %s (%s)\n", version.Version, version.Commit)
		return nil
	}
	if err := api.ValidateListenAddr(*addr); err != nil {
		return err
	}
	cfg := config.Default()
	if *cfgPath != "" {
		data, err := os.ReadFile(*cfgPath)
		if err != nil {
			return err
		}
		if cfg, err = config.Parse(data); err != nil {
			return err
		}
	}
	if *dlDir != "" {
		cfg.Storage.DefaultDir = *dlDir
	}
	if cfg.Storage.DefaultDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		cfg.Storage.DefaultDir = filepath.Join(home, "Downloads", "StreamVault")
	}
	data := *dataDir
	if data == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		data = filepath.Join(base, "StreamVault")
	}
	for _, d := range []string{data, cfg.Storage.DefaultDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	var opts []app.Option
	var be *btnative.Backend
	if !*noTorrent {
		bo := btnative.Options{ListenAddr: *btListen, DHT: !*noDHT, AllowPrivatePeers: *btLAN}
		var err error
		be, err = btnative.New(bo)
		if err != nil { // porta ocupada: usar uma livre em vez de desistir
			bo.ListenAddr = ":0"
			if be, err = btnative.New(bo); err != nil {
				return err
			}
		}
		opts = append(opts, app.WithTorrentBackend(be))
		fmt.Fprintf(out, "BitTorrent ativo (peers em %s, DHT %v)\n", be.Addr(), !*noDHT)
	}
	core, err := open(cfg, filepath.Join(data, "core.db"), opts...)
	if err != nil {
		if be != nil {
			be.Close()
		}
		return err
	}
	defer core.Close()

	// agendador de downloads: a agenda fica ao lado da base de dados. Se o ficheiro
	// estiver ilegível, o daemon arranca na mesma (sem agendador) e avisa, em vez
	// de falhar ou de descartar a agenda.
	if err := core.StartScheduler(filepath.Join(data, "schedule.json"), scheduler.Options{}); err != nil {
		fmt.Fprintf(out, "AVISO: agendador desativado: %v\n", err)
	}

	if *licPath != "" {
		ent, err := loadLicense(*licPath, *licPub, *licKeyID, cfg.Commercial.ProductID)
		if err != nil {
			fmt.Fprintf(out, "Licença não aplicada: %v\n", err)
		} else {
			core.SetEntitlements(ent)
			fmt.Fprintln(out, "Licença válida aplicada.")
		}
	}
	if *lanListen != "" {
		// o daemon só aceita LINKS por LAN: ficheiros exigem confirmação do
		// utilizador na UI (confirmFile nil => recusados)
		lanDir := filepath.Join(cfg.Storage.DefaultDir, "LAN")
		svc, err := core.StartLAN(hostName(), lanDir, *lanListen, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Recetor LAN a escutar em %s\n", svc.Addr)
		if *lanAnnounce {
			_, portStr, _ := net.SplitHostPort(svc.Addr)
			var port int
			fmt.Sscanf(portStr, "%d", &port)
			id := make([]byte, 8)
			rand.Read(id)
			go lan.Beacon(ctx, "", lan.Announcement{ID: hex.EncodeToString(id), Name: hostName(), Port: port}, 2*time.Second)
		}
	}

	token, err := loadOrCreateToken(data, *newToken)
	if err != nil {
		return err
	}
	srv, err := api.New(core, token)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil && *addr == defaultAddr { // porta ocupada (outra instância?): usar uma livre
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	url := fmt.Sprintf("http://%s/#token=%s", ln.Addr().String(), token)
	fmt.Fprintf(out, "StreamVault %s a escutar em %s\nAbra: %s\n", version.Version, ln.Addr(), url)
	if *openBrowser {
		launch(url)
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

func launch(url string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		_ = exec.Command("open", url).Start()
	default:
		_ = exec.Command("xdg-open", url).Start()
	}
}

func hostName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "StreamVault"
}

// loadLicense valida a licença (assinatura, produto, datas) e devolve os direitos.
func loadLicense(path, pubPath, keyID, productID string) (domain.Entitlements, error) {
	if pubPath == "" || keyID == "" {
		return domain.Entitlements{}, errors.New("faltam -license-pub e -license-key-id")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Entitlements{}, err
	}
	l, err := license.Parse(raw)
	if err != nil {
		return domain.Entitlements{}, err
	}
	pb, err := os.ReadFile(pubPath)
	if err != nil {
		return domain.Entitlements{}, err
	}
	pub, err := hex.DecodeString(strings.TrimSpace(string(pb)))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return domain.Entitlements{}, errors.New("chave pública inválida")
	}
	ring := license.NewKeyring()
	if err := ring.Trust(keyID, ed25519.PublicKey(pub)); err != nil {
		return domain.Entitlements{}, err
	}
	res := (&license.Validator{ProductID: productID, Ring: ring}).Validate(l)
	if res.Status != license.StatusValid {
		return domain.Entitlements{}, fmt.Errorf("%s: %s", res.Status, res.Reason)
	}
	e := res.Entitlements
	return domain.Entitlements{AdFree: e.AdFree, PriorityQueue: e.PriorityQueue,
		AdvancedFilters: e.AdvancedFilters, TorrentEngine: e.TorrentEngine, LANTransfer: e.LANTransfer}, nil
}

// loadOrCreateToken devolve a chave de acesso persistente (ficheiro api-token,
// 0600, na pasta de dados), criando-a se não existir. Persistente para que a
// extensão do navegador não precise de ser reconfigurada a cada arranque.
func loadOrCreateToken(dir string, rotate bool) (string, error) {
	path := filepath.Join(dir, "api-token")
	if !rotate {
		if b, err := os.ReadFile(path); err == nil {
			t := strings.TrimSpace(string(b))
			if len(t) >= 32 && isHex(t) {
				return t, nil
			}
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return t, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}
