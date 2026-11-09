package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/docker"
	"github.com/ammonode/daemon/internal/remote"
	"github.com/ammonode/daemon/internal/router"
	"github.com/ammonode/daemon/internal/server"
	"github.com/ammonode/daemon/internal/sftp"
	"github.com/ammonode/daemon/internal/system"
)

var configPath string

func main() {
	root := &cobra.Command{
		Use:   "ammonode-daemon",
		Short: "Runs game servers for the Ammonode panel",
		RunE:  run,
	}
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultPath, "path to the configuration file")
	root.AddCommand(configureCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the daemon version",
		Run:   func(*cobra.Command, []string) { fmt.Println("ammonode-daemon", system.Version) },
	}
}

func configureCmd() *cobra.Command {
	var panelURL, uuid, tokenID, token, sslCert, sslKey string
	var port, sftp int
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Write the configuration file using values from the panel's node page",
		RunE: func(*cobra.Command, []string) error {
			if panelURL == "" || uuid == "" || tokenID == "" || token == "" {
				return errors.New("--panel, --uuid, --token-id and --token are required")
			}
			c := config.New()
			c.Remote, c.UUID, c.TokenID, c.Token = panelURL, uuid, tokenID, token
			c.API.Port = port
			c.System.SFTP.Port = sftp
			// The panel passes certificate paths when the node uses HTTPS and is not behind a proxy.
			if sslCert != "" && sslKey != "" {
				c.API.SSL.Enabled = true
				c.API.SSL.Cert, c.API.SSL.Key = sslCert, sslKey
			}
			if err := c.Save(configPath); err != nil {
				return err
			}
			fmt.Println("Configuration written to", configPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&panelURL, "panel", "", "panel URL, e.g. https://panel.example.com")
	cmd.Flags().StringVar(&uuid, "uuid", "", "node UUID")
	cmd.Flags().StringVar(&tokenID, "token-id", "", "node token id")
	cmd.Flags().StringVar(&token, "token", "", "node token")
	cmd.Flags().IntVar(&port, "port", 8080, "API port")
	cmd.Flags().IntVar(&sftp, "sftp-port", 2022, "SFTP port")
	cmd.Flags().StringVar(&sslCert, "ssl-cert", "", "TLS certificate (enables HTTPS together with --ssl-key)")
	cmd.Flags().StringVar(&sslKey, "ssl-key", "", "TLS private key")
	return cmd
}

func run(*cobra.Command, []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	slog.Info("starting Ammonode daemon", "version", system.Version)

	if err := ensureUser(cfg); err != nil {
		return err
	}
	for _, dir := range []string{cfg.System.RootDirectory, cfg.System.Data, cfg.System.ArchiveDir, cfg.System.BackupDir, cfg.System.TmpDirectory, cfg.System.LogDirectory} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := docker.EnsureNetwork(ctx, cfg); err != nil {
		return fmt.Errorf("docker network: %w", err)
	}

	panel := remote.New(cfg.Remote, cfg.TokenID, cfg.Token)
	manager := server.NewManager(panel)
	if err := manager.Boot(ctx); err != nil {
		slog.Error("could not load servers from the panel, will continue without them", "error", err)
	}
	defer manager.Shutdown()

	go heartbeat(ctx, panel)
	go func() {
		if err := sftp.New(cfg, manager).Run(ctx); err != nil {
			slog.Error("SFTP server stopped", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.API.Host + ":" + strconv.Itoa(cfg.API.Port),
		Handler:           router.New(cfg, manager),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("API listening", "address", srv.Addr, "ssl", cfg.API.SSL.Enabled)
	if cfg.API.SSL.Enabled {
		err = srv.ListenAndServeTLS(cfg.API.SSL.Cert, cfg.API.SSL.Key)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func heartbeat(ctx context.Context, panel *remote.Client) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		info, _ := system.Info(ctx)
		if err := panel.Heartbeat(ctx, info); err != nil {
			slog.Warn("heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
