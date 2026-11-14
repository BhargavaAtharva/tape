package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/BhargavaAtharva/tape/feed"
)

const pingInterval = 20 * time.Second

// handleWS streams one symbol: a snapshot, then every update. The writer
// never blocks the feed: the feed drops a subscriber whose buffer is full,
// and a write that takes longer than WriteTimeout drops it here.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	f, ok := s.feed(w, r)
	if !ok {
		return
	}
	depth, ok := intParam(r, "depth", 50, 1, 1000)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_depth", "depth must be 1..1000")
		return
	}
	opts := &websocket.AcceptOptions{OriginPatterns: s.cfg.Origins, CompressionMode: websocket.CompressionDisabled}
	if s.allowAnyOrigin() {
		opts.InsecureSkipVerify = true
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	sub, err := f.Subscribe(depth, 100)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer f.Unsubscribe(sub)

	// Clients have nothing to say; CloseRead handles control frames and
	// cancels ctx when the peer goes away.
	ctx := conn.CloseRead(r.Context())
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case msg := <-sub.C():
			if err := s.write(ctx, conn, msg); err != nil {
				s.dropSlow(f, sub, conn, err)
				return
			}
		case <-sub.Done():
			slog.Info("ws client dropped", "symbol", f.Symbol(), "reason", sub.Reason().String())
			closeFor(conn, sub.Reason())
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

func (s *Server) dropSlow(f *feed.Feed, sub *feed.Subscriber, conn *websocket.Conn, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		f.Drop(sub, feed.ReasonSlow)
		slog.Info("ws client dropped", "symbol", f.Symbol(), "reason", "write timeout")
		conn.Close(websocket.StatusPolicyViolation, feed.ReasonSlow.String())
	}
}

func closeFor(conn *websocket.Conn, r feed.DropReason) {
	switch r {
	case feed.ReasonSlow:
		conn.Close(websocket.StatusPolicyViolation, r.String())
	case feed.ReasonResync:
		conn.Close(websocket.StatusTryAgainLater, r.String())
	default:
		conn.Close(websocket.StatusGoingAway, r.String())
	}
}
