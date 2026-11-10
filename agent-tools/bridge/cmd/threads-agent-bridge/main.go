package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/t3"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/config"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/daemon"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/store"
)

func main() {
	configPath := flag.String("config", "config.json", "path to JSON config")
	setup := flag.Bool("t3-setup", false, "authorize T3 through headless OAuth pairing")
	check := flag.Bool("t3-check", false, "read T3 projects using saved OAuth credentials")
	origin := flag.String("t3-url", "http://127.0.0.1:3773", "T3 HTTP origin")
	credentials := flag.String("t3-credentials", ".local/t3-credentials.json", "private T3 OAuth credential file")
	command := flag.String("t3-command", "t3", "T3 CLI used only for OAuth pairing")
	flag.Parse()
	if *setup || *check {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var err error
		if *setup {
			err = t3.Setup(ctx, *origin, *credentials, *command)
			if err == nil {
				fmt.Println("T3 authorization saved to " + *credentials)
			}
		}
		if err == nil && *check {
			var client *t3.Client
			client, err = t3.New(ctx, *origin, *credentials)
			if err == nil {
				defer client.Close()
				var projects []byte
				projects, err = client.Check(ctx)
				if err == nil {
					fmt.Println(string(projects))
				}
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(*configPath); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("starting threads agent bridge", "scopes", len(cfg.Scopes), "database", cfg.DatabasePath)
	d := daemon.Daemon{Config: cfg, Store: st}
	return d.Run(ctx)
}
