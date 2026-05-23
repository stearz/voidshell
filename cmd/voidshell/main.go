package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stearz/voidshell/internal/auth"
	"github.com/stearz/voidshell/internal/config"
	"github.com/stearz/voidshell/internal/k8s"
	"github.com/stearz/voidshell/internal/server"
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

	ttl, err := time.ParseDuration(cfg.Auth.KeyCacheTTL)
	if err != nil {
		log.Error("invalid keyCacheTTL", "value", cfg.Auth.KeyCacheTTL, "error", err)
		os.Exit(1)
	}

	authenticator := auth.New(cfg.Auth.AllowedGitHubUsers, ttl, log)

	k8sMgr, err := k8s.NewFromKubeconfig("", k8s.Config{
		Namespace:    cfg.Kubernetes.GuestNamespace,
		StorageClass: cfg.Kubernetes.StorageClass,
		StorageSize:  cfg.Kubernetes.StorageSize,
		ShellImage:   cfg.Workspace.ShellImage,
		ShellCommand: cfg.Workspace.ShellCommand,
	})
	if err != nil {
		log.Error("failed to create Kubernetes manager", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Detect the namespace this pod runs in for loading secrets.
	namespace := podNamespace(cfg.Kubernetes.GuestNamespace)
	hostKey, err := server.LoadHostKey(ctx, cfg.SSH, k8sMgr.Client(), namespace)
	if err != nil {
		log.Error("failed to load host key", "error", err)
		os.Exit(1)
	}

	srv := server.New(hostKey, authenticator, k8sMgr, k8sMgr, log)

	addr := fmt.Sprintf(":%d", cfg.SSH.Port)
	log.Info("voidshell starting",
		"ssh_port", cfg.SSH.Port,
		"guest_namespace", cfg.Kubernetes.GuestNamespace,
		"shell_image", cfg.Workspace.ShellImage,
	)

	if err := srv.ListenAndServe(ctx, addr); err != nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	log.Info("voidshell shutting down")
}

// podNamespace returns the Kubernetes namespace this pod is running in.
// It reads the standard downward-API file; falls back to defaultNS when
// not running in-cluster.
func podNamespace(defaultNS string) string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil && len(data) > 0 {
		return string(data)
	}
	return defaultNS
}
