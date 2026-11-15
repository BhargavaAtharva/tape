// Package api is the HTTP and WebSocket gateway in front of the exchange.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/feed"
)

// Symbol is a tradable instrument. Prices travel as integer ticks; the
// client divides by 10^PriceScale to display them.
type Symbol struct {
	Name       string `json:"symbol"`
	PriceScale int    `json:"price_scale"`
	// Ref is the reference price in ticks that the static band is centred
	// on; zero disables the static band.
	Ref int64 `json:"reference"`
}

type Config struct {
	Symbols []Symbol
	// Origins lists allowed browser origins for CORS and WebSockets. "*"
	// allows any.
	Origins []string
	// RateLimit is order-entry requests per second per client address, with
	// a burst of twice that. Zero disables limiting.
	RateLimit float64
	// MinTrader is the lowest trader id clients may use; ids below it are
	// reserved for the in-process simulator.
	MinTrader uint64
	// TrustProxy takes the client address from X-Forwarded-For.
	TrustProxy bool
	// WriteTimeout bounds one WebSocket write; a client that cannot take a
	// message within it is dropped.
	WriteTimeout time.Duration
	Risk         Risk
	// MaxConns and MaxConnsPerAddr cap open WebSocket connections in total
	// and per client address.
	MaxConns        int
	MaxConnsPerAddr int
	// Info is extra static information served on /v1/stats, such as the WAL
	// sync policy and whether the simulator is running.
	Info map[string]string
}

type Server struct {
	cfg     Config
	x       *engine.Exchange
	feeds   map[string]*feed.Feed
	reports *feed.Reports
	conns   connLimiter
	limiter *limiter
	stats   *sampler
	mux     *http.ServeMux
}

func New(cfg Config, x *engine.Exchange, feeds map[string]*feed.Feed, reports *feed.Reports) *Server {
	cfg.MinTrader = max(cfg.MinTrader, 1)
	cfg.Risk.setDefaults()
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 500
	}
	if cfg.MaxConnsPerAddr <= 0 {
		cfg.MaxConnsPerAddr = 8
	}
	s := &Server{cfg: cfg, x: x, feeds: feeds, reports: reports, stats: newSampler(x, feeds), mux: http.NewServeMux()}
	s.conns = connLimiter{max: cfg.MaxConns, perAddr: cfg.MaxConnsPerAddr, byAddr: make(map[string]int)}
	if cfg.RateLimit > 0 {
		s.limiter = newLimiter(cfg.RateLimit, 2*cfg.RateLimit)
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	s.mux.HandleFunc("GET /v1/symbols", s.handleSymbols)
	s.mux.HandleFunc("GET /v1/book/{symbol}", s.handleBook)
	s.mux.HandleFunc("GET /v1/trades/{symbol}", s.handleTrades)
	s.mux.HandleFunc("GET /v1/stats", s.handleStats)
	s.mux.HandleFunc("GET /v1/orders", s.handleOpenOrders)
	s.mux.HandleFunc("POST /v1/orders", s.limited(s.handleNewOrder))
	s.mux.HandleFunc("DELETE /v1/orders/{id}", s.limited(s.handleCancel))
	s.mux.HandleFunc("PATCH /v1/orders/{id}", s.limited(s.handleAmend))
	s.mux.HandleFunc("GET /v1/ws/private", s.handlePrivateWS)
	s.mux.HandleFunc("GET /v1/ws/{symbol}", s.handleWS)
	return s
}

// Run samples engine statistics until ctx is done.
func (s *Server) Run(ctx context.Context) { s.stats.run(ctx) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) allowAnyOrigin() bool { return slices.Contains(s.cfg.Origins, "*") }

// cors sets CORS headers and reports whether the request was a preflight
// that has been fully answered.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	switch {
	case s.allowAnyOrigin():
		w.Header().Set("Access-Control-Allow-Origin", "*")
	case slices.Contains(s.cfg.Origins, origin):
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	default:
		return false
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Trader-ID")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("response write failed", "err", err)
	}
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

// engineError maps a failure to reach an engine onto a response.
func engineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "timeout", "engine did not respond in time")
	case errors.Is(err, engine.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "engine is shutting down")
	default:
		slog.Error("engine unavailable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "engine_failed", "engine is unavailable")
	}
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request) (*feed.Feed, bool) {
	f, ok := s.feeds[r.PathValue("symbol")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_symbol", "no such symbol")
	}
	return f, ok
}

// intParam parses an optional integer query parameter within [lo, hi].
func intParam(r *http.Request, name string, def, lo, hi int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

type symbolInfo struct {
	Symbol
	feed.Ticker
	band
	MaxQty      int64   `json:"max_qty"`
	MaxNotional float64 `json:"max_notional"`
}

func (s *Server) handleSymbols(w http.ResponseWriter, _ *http.Request) {
	out := make([]symbolInfo, 0, len(s.cfg.Symbols))
	for _, sym := range s.cfg.Symbols {
		out = append(out, symbolInfo{Symbol: sym, Ticker: s.feeds[sym.Name].Ticker(), band: s.band(sym),
			MaxQty: s.cfg.Risk.MaxQty, MaxNotional: s.cfg.Risk.MaxNotional})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feed(w, r)
	if !ok {
		return
	}
	depth, ok := intParam(r, "depth", 20, 1, 1000)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_depth", "depth must be 1..1000")
		return
	}
	snap := f.Book(depth)
	snap.Type = "book"
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feed(w, r)
	if !ok {
		return
	}
	limit, ok := intParam(r, "limit", 100, 1, 500)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..500")
		return
	}
	writeJSON(w, http.StatusOK, f.Trades(limit))
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	st := s.stats.current()
	st.Info = s.cfg.Info
	writeJSON(w, http.StatusOK, st)
}

// State is the resync source for a feed: the engine's full L2 book.
func State(e *engine.Engine) func(context.Context) (feed.State, error) {
	return func(ctx context.Context) (feed.State, error) {
		var st feed.State
		err := e.Query(ctx, func(b *book.Book) {
			st = feed.State{Seq: b.Seq(), Bids: b.Depth(book.Buy, 0), Asks: b.Depth(book.Sell, 0)}
		})
		return st, err
	}
}
