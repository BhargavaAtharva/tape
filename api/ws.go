package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/BhargavaAtharva/tape/feed"
)

const pingInterval = 20 * time.Second

// connLimiter caps WebSocket connections in total and per client address.
type connLimiter struct {
	max, perAddr int

	mu     sync.Mutex
	total  int
	byAddr map[string]int
}

func (c *connLimiter) acquire(addr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total >= c.max || c.byAddr[addr] >= c.perAddr {
		return false
	}
	c.total++
	c.byAddr[addr]++
	return true
}

func (c *connLimiter) release(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total--
	if c.byAddr[addr]--; c.byAddr[addr] == 0 {
		delete(c.byAddr, addr)
	}
}

// accept upgrades the request if the connection limits allow it. The
// returned release must be called when the connection ends.
func (s *Server) accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, func(), bool) {
	addr := s.clientAddr(r)
	if !s.conns.acquire(addr) {
		writeError(w, http.StatusTooManyRequests, "too_many_connections", "too many open streams")
		return nil, nil, false
	}
	// Market data is repetitive JSON; per-message deflate with a shared
	// window cuts it several times over, which matters on metered egress.
	opts := &websocket.AcceptOptions{OriginPatterns: s.cfg.Origins, CompressionMode: websocket.CompressionContextTakeover}
	if s.allowAnyOrigin() {
		opts.InsecureSkipVerify = true
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.conns.release(addr)
		return nil, nil, false
	}
	return conn, func() { conn.CloseNow(); s.conns.release(addr) }, true
}

// handleWS streams one symbol: a full snapshot, then conflated updates.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feed(w, r)
	if !ok {
		return
	}
	conn, release, ok := s.accept(w, r)
	if !ok {
		return
	}
	defer release()
	sub, err := f.Subscribe()
	if err != nil {
		conn.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer f.Unsubscribe(sub)
	s.stream(r.Context(), conn, sub, f.Symbol(), func() { f.Drop(sub, feed.ReasonSlow) })
}

// handlePrivateWS streams execution reports for one trader. Like the rest
// of the API it trusts the trader id it is given: there are no accounts.
func (s *Server) handlePrivateWS(w http.ResponseWriter, r *http.Request) {
	trader, err := strconv.ParseUint(r.URL.Query().Get("trader"), 10, 64)
	if err != nil || trader < s.cfg.MinTrader || trader > maxTrader {
		writeError(w, http.StatusBadRequest, "invalid_trader", "trader must be an id the gateway accepts")
		return
	}
	conn, release, ok := s.accept(w, r)
	if !ok {
		return
	}
	defer release()
	sub := s.reports.Subscribe(trader)
	defer s.reports.Unsubscribe(trader, sub)
	s.stream(r.Context(), conn, sub, "private", func() {})
}

// stream writes a subscriber's messages until either side goes away. The
// writer never blocks the feed: the feed drops a subscriber whose buffer is
// full, and a write that takes longer than WriteTimeout drops it here.
func (s *Server) stream(ctx context.Context, conn *websocket.Conn, sub *feed.Subscriber, name string, dropSlow func()) {
	// Clients have nothing to say; CloseRead handles control frames and
	// cancels ctx when the peer goes away.
	ctx = conn.CloseRead(ctx)
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case msg := <-sub.C():
			if err := s.write(ctx, conn, msg); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					dropSlow()
					slog.Info("ws client dropped", "stream", name, "reason", "write timeout")
					conn.Close(websocket.StatusPolicyViolation, feed.ReasonSlow.String())
				}
				return
			}
		case <-sub.Done():
			if reason := sub.Reason(); reason != feed.ReasonNone {
				slog.Info("ws client dropped", "stream", name, "reason", reason.String())
				code := websocket.StatusGoingAway
				if reason == feed.ReasonSlow {
					code = websocket.StatusPolicyViolation
				}
				conn.Close(code, reason.String())
			}
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) write(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, msg)
}
