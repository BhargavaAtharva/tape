// Command tape runs the matching engine with its HTTP/WebSocket gateway.
// Configuration comes from the environment; see .env.example.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
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
	"github.com/BhargavaAtharva/tape/feed"
	"github.com/BhargavaAtharva/tape/sim"
	"github.com/BhargavaAtharva/tape/wal"
)

type config struct {
	addr          string
	symbols       []api.Symbol
	risk          api.Risk
	dataDir       string
	walSync       wal.SyncPolicy
	walInterval   time.Duration
	snapshotEvery uint64
	origins       []string
	rateLimit     float64
	proxyHops     int
	feedBuffer    int
	feedInterval  time.Duration
	sim           bool
	simIntensity  float64
	simSeed       uint64
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
		origins: splitList(env("TAPE_CORS_ORIGINS", "*")),
	}
	var err error
	if c.symbols, err = parseSymbols(env("TAPE_SYMBOLS", "BTC-USD:2:64250,ETH-USD:2:3120,SOL-USD:3:148.5,NIFTY:2:24350")); err != nil {
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
	if c.proxyHops, err = strconv.Atoi(env("TAPE_PROXY_HOPS", "0")); err != nil || c.proxyHops < 0 {
		return c, fmt.Errorf("TAPE_PROXY_HOPS: want a non-negative integer")
	}
	if c.feedBuffer, err = strconv.Atoi(env("TAPE_FEED_BUFFER", "512")); err != nil {
		return c, fmt.Errorf("TAPE_FEED_BUFFER: %w", err)
	}
	if c.risk.CollarPct, err = strconv.ParseFloat(env("TAPE_COLLAR_PCT", "10"), 64); err != nil {
		return c, fmt.Errorf("TAPE_COLLAR_PCT: %w", err)
	}
	if c.risk.StaticPct, err = strconv.ParseFloat(env("TAPE_STATIC_BAND_PCT", "25"), 64); err != nil {
		return c, fmt.Errorf("TAPE_STATIC_BAND_PCT: %w", err)
	}
	if c.risk.MaxQty, err = strconv.ParseInt(env("TAPE_MAX_ORDER_QTY", "10000"), 10, 64); err != nil {
		return c, fmt.Errorf("TAPE_MAX_ORDER_QTY: %w", err)
	}
	if c.risk.MaxNotional, err = strconv.ParseFloat(env("TAPE_MAX_NOTIONAL", "5000000"), 64); err != nil {
		return c, fmt.Errorf("TAPE_MAX_NOTIONAL: %w", err)
	}
	if c.risk.OrderTTL, err = time.ParseDuration(env("TAPE_ORDER_TTL", "30m")); err != nil {
		return c, fmt.Errorf("TAPE_ORDER_TTL: %w", err)
	}
	if c.feedInterval, err = time.ParseDuration(env("TAPE_FEED_INTERVAL", "100ms")); err != nil {
		return c, fmt.Errorf("TAPE_FEED_INTERVAL: %w", err)
	}
	if c.sim, err = strconv.ParseBool(env("TAPE_SIM", "true")); err != nil {
		return c, fmt.Errorf("TAPE_SIM: %w", err)
	}
	if c.simSeed, err = strconv.ParseUint(env("TAPE_SIM_SEED", "0"), 10, 64); err != nil {
		return c, fmt.Errorf("TAPE_SIM_SEED: %w", err)
	}
	if c.simSeed == 0 {
		c.simSeed = uint64(time.Now().UnixNano())
	}
	if c.simIntensity, err = strconv.ParseFloat(env("TAPE_SIM_INTENSITY", "0.5"), 64); err != nil {
		return c, fmt.Errorf("TAPE_SIM_INTENSITY: %w", err)
	}
	return c, nil
}

// parseSymbols reads "NAME:scale:ref,..." where ref is the simulator's
// reference price in display units. The order matters once a data
// directory exists, because a symbol's position is part of its order ids.
func parseSymbols(s string) ([]api.Symbol, error) {
	var syms []api.Symbol
	for item := range strings.SplitSeq(s, ",") {
		parts := strings.Split(strings.TrimSpace(item), ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("TAPE_SYMBOLS: %q is not NAME:price_scale:reference_price", item)
		}
		scale, err := strconv.Atoi(parts[1])
		if err != nil || scale < 0 || scale > 8 {
			return nil, fmt.Errorf("TAPE_SYMBOLS: %q: price scale must be 0..8", item)
		}
		ref, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || ref <= 0 {
			return nil, fmt.Errorf("TAPE_SYMBOLS: %q: reference price must be positive", item)
		}
		syms = append(syms, api.Symbol{Name: parts[0], PriceScale: scale, Ref: int64(math.Round(ref * math.Pow10(scale)))})
	}
	return syms, nil
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
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
		Symbols:   cfg.symbols,
		Risk:      cfg.risk,
		Origins:   cfg.origins,
		RateLimit: cfg.rateLimit,
		ProxyHops: cfg.proxyHops,
		MinTrader: sim.MaxBotTrader + 1,
		Info:      map[string]string{"wal_sync": durability, "simulator": strconv.FormatBool(cfg.sim)},
	}, feed.Options{Buffer: cfg.feedBuffer, Interval: cfg.feedInterval}, func(c *engine.Config) {
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

	simCtx, stopSim := context.WithCancel(context.Background())
	simDone := make(chan struct{})
	if cfg.sim {
		markets := make([]sim.Market, len(cfg.symbols))
		for i, s := range cfg.symbols {
			markets[i] = sim.Market{Symbol: s.Name, Ref: s.Ref}
		}
		go func() {
			defer close(simDone)
			if err := sim.Run(simCtx, st.Exchange, sim.Config{Markets: markets, Intensity: cfg.simIntensity, Seed: cfg.simSeed}); err != nil {
				slog.Error("simulator stopped", "err", err)
			}
		}()
	} else {
		close(simDone)
	}
	shutdownSim := func() {
		stopSim()
		<-simDone
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           st.Server,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		shutdownSim()
		st.Close()
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("listening", "addr", cfg.addr, "symbols", len(cfg.symbols), "wal", durability, "data", cfg.dataDir, "sim", cfg.sim)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		shutdownSim()
		st.Close()
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownSim()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("http shutdown", "err", err)
	}
	return st.Close()
}
