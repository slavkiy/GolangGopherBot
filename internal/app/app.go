package app

import (
	"context"
	"golanggopherbot/internal/bot"
	"golanggopherbot/internal/config"
	"golanggopherbot/internal/store"
)

type App struct {
	bot   *bot.Bot
	store *store.Store
}

func New(cfg config.Config) (*App, error) {
	s, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	b, err := bot.New(cfg, s)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &App{bot: b, store: s}, nil
}
func (a *App) Run(ctx context.Context) error { return a.bot.Run(ctx) }
func (a *App) Close() error                  { return a.store.Close() }
