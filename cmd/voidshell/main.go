package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stearz/voidshell/internal/config"
)

func main() {
	configPath := flag.String("config", os.Getenv("VOIDSHELL_CONFIG"), "path to YAML config file (env: VOIDSHELL_CONFIG)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	log.Info("voidshell starting",
		"ssh_port", cfg.SSH.Port,
		"guest_namespace", cfg.Kubernetes.GuestNamespace,
		"shell_image", cfg.Workspace.ShellImage,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	log.Info("voidshell shutting down")
}
