package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	settings := Settings{
		Addr:             envOrDefault("EDKA_CODEX_PROXY_ADDR", ":4500"),
		Upstream:         envOrDefault("EDKA_CODEX_PROXY_UPSTREAM", "ws://127.0.0.1:4501"),
		BrokerURL:        os.Getenv("EDKA_CODEX_BROKER_URL"),
		BrokerTokenFile:  envOrDefault("EDKA_CODEX_BROKER_TOKEN_FILE", "/var/run/edka/broker/token"),
		SharedSecretFile: envOrDefault("EDKA_CODEX_WS_SHARED_SECRET_FILE", "/var/run/edka/ws/secret"),
		Issuer:           envOrDefault("EDKA_CODEX_WS_ISSUER", "edka"),
		Audience:         os.Getenv("EDKA_CODEX_WS_AUDIENCE"),
	}

	flags := flag.NewFlagSet("codex-proxy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&settings.Addr, "addr", settings.Addr, "HTTP/WebSocket listen address")
	flags.StringVar(&settings.Upstream, "upstream", settings.Upstream, "Codex app-server WebSocket URL")
	flags.StringVar(&settings.BrokerURL, "broker-url", settings.BrokerURL, "Edka subscription broker URL")
	flags.StringVar(&settings.BrokerTokenFile, "broker-token-file", settings.BrokerTokenFile, "broker bearer-token file")
	flags.StringVar(&settings.SharedSecretFile, "ws-shared-secret-file", settings.SharedSecretFile, "remote-auth HMAC secret file")
	flags.StringVar(&settings.Issuer, "ws-issuer", settings.Issuer, "required remote-auth token issuer")
	flags.StringVar(&settings.Audience, "ws-audience", settings.Audience, "required remote-auth token audience")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := settings.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	proxy := New(settings, logger)
	if err := proxy.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func envOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
