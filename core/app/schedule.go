package app

import (
	"errors"
	"time"

	"streamvault/core/domain"
	"streamvault/core/scheduler"
)

// StartScheduler liga o agendador de downloads (ADR-010), guardado em path
// (JSON, 0600). Chamar uma vez, depois de New. À hora marcada a entrada passa por
// AddWith, logo herda segurança, fila e retoma. Entradas já vencidas ao arrancar
// correm de imediato. Um ficheiro ilegível devolve erro (não se perde a agenda).
func (c *Core) StartScheduler(path string, o scheduler.Options) error {
	if c.Scheduler != nil {
		return errors.New("app: o agendador já foi iniciado")
	}
	s, err := scheduler.New(path, func(e scheduler.Entry) error {
		_, err := c.AddWith(e.URL, domain.DownloadPolicy{}, Hints{Referer: e.Referer, UserAgent: e.UserAgent, Cookie: e.Cookie})
		return err
	}, o)
	if err != nil {
		return err
	}
	c.Scheduler = s
	return nil
}

// Schedule agenda o início de um download. Valida AGORA (segurança, formato,
// cookie) para o utilizador saber já se o link seria recusado, em vez de só à
// hora marcada. O cookie fica só em memória.
func (c *Core) Schedule(raw string, at time.Time, h Hints) (scheduler.Entry, error) {
	if c.Scheduler == nil {
		return scheduler.Entry{}, errors.New("o agendador não está ativo neste dispositivo")
	}
	if err := c.Validate(raw, h); err != nil {
		return scheduler.Entry{}, err
	}
	return c.Scheduler.Add(scheduler.Entry{URL: raw, Referer: h.Referer, UserAgent: h.UserAgent, At: at, Cookie: h.Cookie})
}
