// server starts the D03 webhook ingestion service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gogf/gf/v2/frame/g"

	"oncall-agent/internal/api"
	"oncall-agent/internal/config"
	"oncall-agent/internal/ingest"
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
	if strings.TrimSpace(cfg.Server.AuthToken) == "" {
		return errors.New("config: server.auth_token is required for webhook server")
	}

	db, err := store.Open(cfg.MySQL.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	worker := ingest.NewWorker(db, cfg.Ingest, log.Default())
	if err := worker.Start(ctx); err != nil {
		return err
	}

	server := g.Server()
	server.SetPort(cfg.Server.Port)
	server.SetGraceful(true)
	server.BindHandler("/webhook/alertmanager", api.NewAlertmanagerWebhook(db, worker, cfg.Server.AuthToken).Handle)
	if err := server.Start(); err != nil {
		stop()
		worker.Wait()
		return fmt.Errorf("server: start HTTP listener: %w", err)
	}
	fmt.Fprintln(os.Stdout, "oncall-agent: server ready")

	<-ctx.Done()
	shutdownErr := server.Shutdown()
	stop()
	worker.Wait()
	return shutdownErr
}
