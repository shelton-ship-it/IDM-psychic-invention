package app

import (
	"errors"
	"net"
	"sync"
	"time"

	"streamvault/core/domain"
	"streamvault/core/lan"
)

// SetEntitlements define os direitos da licença validada (a UI chama-o depois
// de validar a licença). Sem isto, funcionalidades licenciadas ficam fechadas.
func (c *Core) SetEntitlements(e domain.Entitlements) {
	c.entMu.Lock()
	c.ent = e
	c.entMu.Unlock()
}

func (c *Core) entitlements() domain.Entitlements {
	c.entMu.Lock()
	defer c.entMu.Unlock()
	return c.ent
}

// LANService é o recetor LAN em execução.
type LANService struct {
	Receiver *lan.Receiver
	Addr     string // host:porta TCP onde escuta
	once     sync.Once
}

// Close para o serviço.
func (s *LANService) Close() { s.once.Do(func() { s.Receiver.Close() }) }

// StartLAN liga o recetor LAN. Links recebidos passam por Core.Add (reconhecimento
// + segurança + política); ficheiros só são aceites se confirmFile existir e
// devolver true (decisão explícita do utilizador). A funcionalidade exige o
// direito LANTransfer da licença.
func (c *Core) StartLAN(name, dir, listen string, confirmFile func(peer, file string, size int64) bool) (*LANService, error) {
	if c.LAN != nil {
		return nil, errors.New("app: LAN já ativo")
	}
	r, err := lan.NewReceiver(lan.ReceiverOptions{
		Name: name, Dir: dir,
		Gate: func() bool { return c.entitlements().LANTransfer },
		OnLink: func(peer, link string) bool {
			_, err := c.Add(link, domain.DownloadPolicy{})
			return err == nil
		},
		OnFile: confirmFile,
	})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	s := &LANService{Receiver: r, Addr: ln.Addr().String()}
	go r.Serve(ln)
	c.LAN = s
	return s, nil
}

// LANPairing abre uma janela de pareamento e devolve o PIN a mostrar na UI.
func (c *Core) LANPairing() (pin string, expires time.Time, err error) {
	if c.LAN == nil {
		return "", time.Time{}, errors.New("app: LAN não está ativo")
	}
	if !c.entitlements().LANTransfer {
		return "", time.Time{}, lan.ErrNotEntitled
	}
	pin, expires = c.LAN.Receiver.StartPairing()
	return pin, expires, nil
}
