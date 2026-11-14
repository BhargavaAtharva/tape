// Command tape runs the matching engine with its HTTP/WebSocket gateway.
// Configuration comes from the environment; see .env.example.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BhargavaAtharva/tape/api"
	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/wal"
)

type config struct {
	addr          string
	symbols       []api.Symbol
	dataDir       string
	walSync       wal.SyncPolicy
	walInterval   time.Duration
	snapshotEvery uint64
	origins       []string
	rateLimit     float64
	trustProxy    bool
	feedBuffer    int
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		addr:    ":" + env("PORT", "8081"),
		dataDir: env("TAPE_DATA_DIR", "data"),
		origins: strings.Split(env("TAPE_CORS_ORIGINS", "*"), ","),
	}
	var err error
	if c.symbols, err = parseSymbols(env("TAPE_SYMBOLS", "BTC-USD:2,ETH-USD:2,SOL-USD:3,TAPE:2")); err != nil {
		return c, err
	}
	if c.walSync, err = wal.ParseSyncPolicy(env("TAPE_WAL_SYNC", "batch")); err != nil {
		return c, err
	}
	if c.walInterval, err = time.ParseDuration(env("TAPE_WAL_SYNC_INTERVAL", "10ms")); err != nil {
		return c, fmt.Errorf("TAPE_WAL_SYNC_INTERVAL: %w", err)
	}
	if c.snapshotEvery, err = strconv.ParseUint(env("TAPE_SNAPSHOT_EVERY", "50000"), 10, 64); err != nil {
		return c, fmt.Errorf("TAPE_SNAPSHOT_EVERY: %w", err)
	}
	if c.rateLimit, err = strconv.ParseFloat(env("TAPE_RATE_LIMIT", "20"), 64); err != nil {
		return c, fmt.Errorf("TAPE_RATE_LIMIT: %w", err)
	}
	if c.trustProxy, err = strconv.ParseBool(env("TAPE_TRUST_PROXY", "false")); err != nil {
		return c, fmt.Errorf("TAPE_TRUST_PROXY: %w", err)
	}
	if c.feedBuffer, err = strconv.Atoi(env("TAPE_FEED_BUFFER", "512")); err != nil {
		return c, fmt.Errorf("TAPE_FEED_BUFFER: %w", err)
	}
	return c, nil
}

// parseSymbols reads "NAME:scale,NAME:scale". The order matters once a data
// directory exists, because a symbol's position is part of its order ids.
func parseSymbols(s string) ([]api.Symbol, error) {
	var out []api.Symbol
	for item := range strings.SplitSeq(s, ",") {
		name, scale, ok := strings.Cut(strings.TrimSpace(item), ":")
		n, err := strconv.Atoi(scale)
		if !ok || err != nil || n < 0 || n > 8 {
			return nil, fmt.Errorf("TAPE_SYMBOLS: %q is not NAME:price_scale (scale 0..8)", item)
		}
		out = append(out, api.Symbol{Name: name, PriceScale: n})
	}
	return out, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	durability := "memory"
	if cfg.dataDir != "" {
		durability = cfg.walSync.String()
	}
	st, err := api.Start(api.Config{
		Symbols:    cfg.symbols,
		Origins:    cfg.origins,
		RateLimit:  cfg.rateLimit,
		TrustProxy: cfg.trustProxy,
		Info:       map[string]string{"wal_sync": durability},
	}, cfg.feedBuffer, func(c *engine.Config) {
		if cfg.dataDir != "" {
			c.Dir = filepath.Join(cfg.dataDir, c.Symbol)
			c.WAL = wal.Options{Sync: cfg.walSync, Interval: cfg.walInterval}
			c.SnapshotEvery = cfg.snapshotEvery
		}
	})
	if err != nil {
		return err
	}
	for _, e := range st.Exchange.Engines() {
		if r := e.Recovery(); r.LastIndex > 0 {
			slog.Info("recovered", "symbol", e.Symbol(), "snapshot", r.SnapshotIndex, "replayed", r.Replayed, "took", r.Duration)
		}
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           st.Server,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.addr, "symbols", len(cfg.symbols), "wal", durability, "data", cfg.dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		st.Close()
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("http shutdown", "err", err)
	}
	return st.Close()
}
