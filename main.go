package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"golanggopherbot/internal/app"
	"golanggopherbot/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer application.Close()

	if err := application.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
