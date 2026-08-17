package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "oncall-agent server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.MySQL.DSN)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintln(os.Stdout, "oncall-agent: server ready")
	<-ctx.Done()
	return db.Close()
}
