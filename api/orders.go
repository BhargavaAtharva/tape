package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
)

// Trader ids must survive a round trip through a JavaScript number.
const maxTrader = 1<<53 - 1

const (
	submitTimeout = 5 * time.Second
	// detachedTimeout bounds a client-order-id submission that outlives its
	// request.
	detachedTimeout = time.Minute
)

type eventJSON struct {
	Seq            uint64 `json:"seq"`
	Time           int64  `json:"time"`
	Kind           string `json:"kind"`
	Side           string `json:"side,omitempty"`
	Reason         string `json:"reason,omitempty"`
	OrderID        uint64 `json:"order_id,omitempty"`
	MakerOrderID   uint64 `json:"maker_order_id,omitempty"`
	Price          int64  `json:"price,omitempty"`
	Qty            int64  `json:"qty"`
	Remaining      int64  `json:"remaining,omitempty"`
	MakerRemaining int64  `json:"maker_remaining,omitempty"`
}

func toJSON(e book.Event) eventJSON {
	j := eventJSON{
		Seq: e.Seq, Time: e.Time / 1e6, Kind: e.Kind.String(), Reason: e.Reason.String(),
		OrderID: e.OrderID, MakerOrderID: e.MakerOrderID, Price: e.Price, Qty: e.Qty,
		Remaining: e.Remaining, MakerRemaining: e.MakerRemaining,
	}
	if e.Side != 0 {
		j.Side = e.Side.String()
	}
	return j
}

// orderResult summarises what one command did to the order it was about.
type orderResult struct {
	OrderID       uint64      `json:"order_id"`
	ClientOrderID string      `json:"client_order_id,omitempty"`
	Symbol        string      `json:"symbol"`
	Status        string      `json:"status"`
	Reason        string      `json:"reason,omitempty"`
	Filled        int64       `json:"filled"`
	Remaining     int64       `json:"remaining"`
	Events        []eventJSON `json:"events"`
}

// summarise reports on the order a command targeted; for a new order the
// target is zero and taken from the Accepted event.
func summarise(symbol string, target uint64, evs []book.Event) (orderResult, book.Reason) {
	res := orderResult{OrderID: target, Symbol: symbol, Events: make([]eventJSON, len(evs))}
	var open int64
	status := ""
	var reject book.Reason
	for i, e := range evs {
		res.Events[i] = toJSON(e)
		switch e.Kind {
		case book.EvRejected:
			status, reject = "rejected", e.Reason
			res.Reason = e.Reason.String()
		case book.EvAccepted, book.EvAmended:
			res.OrderID, open = e.OrderID, e.Qty
		case book.EvFill:
			if e.OrderID == res.OrderID {
				res.Filled += e.Qty
				open -= e.Qty
			}
		case book.EvCancelled:
			if e.OrderID == res.OrderID {
				status = "cancelled"
				res.Reason = e.Reason.String()
				open -= e.Qty
			}
		}
	}
	switch {
	case status != "":
	case open > 0:
		status = "resting"
	default:
		status = "filled"
	}
	res.Status, res.Remaining = status, max(open, 0)
	return res, reject
}

func rejectStatus(r book.Reason) int {
	switch r {
	case book.ReasonNone:
		return http.StatusOK
	case book.ReasonUnknownOrder:
		return http.StatusNotFound
	case book.ReasonNotOwner:
		return http.StatusForbidden
	}
	return http.StatusUnprocessableEntity
}

func (s *Server) traderID(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	id, err := strconv.ParseUint(r.Header.Get("X-Trader-ID"), 10, 64)
	if err != nil || id < s.cfg.MinTrader || id > maxTrader {
		writeError(w, http.StatusUnauthorized, "invalid_trader", fmt.Sprintf("X-Trader-ID must be an integer from %d to 2^53-1", s.cfg.MinTrader))
		return 0, false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		var typeErr *json.UnmarshalTypeError
		var syntaxErr *json.SyntaxError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is limited to 4 KB")
		case errors.As(err, &typeErr):
			writeError(w, http.StatusBadRequest, "invalid_body", fmt.Sprintf("field %q has the wrong type", typeErr.Field))
		case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "invalid_body", "body is not a JSON object")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			writeError(w, http.StatusBadRequest, "invalid_body", strings.TrimPrefix(err.Error(), "json: "))
		default:
			writeError(w, http.StatusBadRequest, "invalid_body", "body could not be read")
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_body", "trailing data after JSON object")
		return false
	}
	return true
}

type newOrderRequest struct {
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Type     string `json:"type"`
	TIF      string `json:"tif"`
	PostOnly bool   `json:"post_only"`
	Price    int64  `json:"price"`
	Qty      int64  `json:"qty"`
	// ClientOrderID, if set, makes the request idempotent per trader: a
	// resend with the same id returns the first outcome.
	ClientOrderID string `json:"client_order_id"`
}

func validClientID(id string) bool {
	if len(id) > 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// command translates the wire names. Range checks on price and quantity are
// left to the book so there is exactly one definition of a valid order.
func (o newOrderRequest) command(trader uint64) (book.Command, string) {
	c := book.Command{Kind: book.CmdNew, Trader: trader, PostOnly: o.PostOnly, Price: o.Price, Qty: o.Qty}
	switch o.Side {
	case "buy":
		c.Side = book.Buy
	case "sell":
		c.Side = book.Sell
	default:
		return c, "side must be buy or sell"
	}
	switch o.Type {
	case "", "limit":
		c.Type, c.TIF = book.Limit, book.GTC
	case "market":
		c.Type, c.TIF = book.Market, book.IOC
	default:
		return c, "type must be limit or market"
	}
	switch o.TIF {
	case "":
	case "gtc":
		c.TIF = book.GTC
	case "ioc":
		c.TIF = book.IOC
	case "fok":
		c.TIF = book.FOK
	default:
		return c, "tif must be gtc, ioc or fok"
	}
	return c, ""
}

func (s *Server) handleNewOrder(w http.ResponseWriter, r *http.Request) {
	trader, ok := s.traderID(w, r)
	if !ok {
		return
	}
	var req newOrderRequest
	if !decodeBody(w, r, &req) {
		return
	}
	e, ok := s.x.Engine(req.Symbol)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_symbol", "no such symbol")
		return
	}
	if !validClientID(req.ClientOrderID) {
		writeError(w, http.StatusBadRequest, "invalid_client_order_id", "client_order_id is up to 36 letters, digits, '-' or '_'")
		return
	}
	cmd, msg := req.command(trader)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_order", msg)
		return
	}
	if book.Validate(cmd) == book.ReasonNone {
		sym, _ := s.symbol(req.Symbol)
		if rerr := s.checkNew(sym, &cmd); rerr != nil {
			writeRisk(w, rerr)
			return
		}
	}
	if req.ClientOrderID == "" {
		s.submit(w, r, e, cmd)
		return
	}
	key := clientKey{trader, req.ClientOrderID}
	entry, owner := s.dedupe.claim(key, time.Now())
	if owner {
		// The submission runs detached from this request, so that if the
		// client gives up waiting the outcome is still recorded for its retry.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), detachedTimeout)
			defer cancel()
			resp, reached := s.execute(ctx, e, cmd)
			if res, ok := resp.body.(orderResult); ok {
				res.ClientOrderID = req.ClientOrderID
				resp.body = res
			}
			if reached {
				s.dedupe.finish(entry, resp)
			} else {
				s.dedupe.forget(key, entry, resp)
			}
		}()
	}
	select {
	case <-entry.done:
		if !owner {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		writeJSON(w, entry.resp.status, entry.resp.body)
	case <-time.After(submitTimeout):
		writePending(w)
	case <-r.Context().Done():
	}
}

// writePending answers a request whose command was queued but whose result
// did not come back in time. The order may still execute; with a
// client_order_id the client can resend to learn the outcome.
func writePending(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "pending", "outcome unknown: the order may still execute; resend with the same client_order_id to learn the result")
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request, e *engine.Engine, cmd book.Command) {
	ctx, cancel := context.WithTimeout(r.Context(), submitTimeout)
	defer cancel()
	resp, _ := s.execute(ctx, e, cmd)
	writeJSON(w, resp.status, resp.body)
}

// execute sends a command without waiting for queue space and reports the
// response along with whether the command reached the engine, in which case
// it will execute whether or not its result arrives before ctx ends.
func (s *Server) execute(ctx context.Context, e *engine.Engine, cmd book.Command) (response, bool) {
	res, err := e.TrySubmit(ctx, cmd)
	switch {
	case err == nil:
		out, reason := summarise(e.Symbol(), cmd.OrderID, res.Events)
		return response{rejectStatus(reason), out}, true
	case errors.Is(err, engine.ErrBusy):
		return response{http.StatusTooManyRequests, apiError{"busy", "the engine is saturated; retry shortly"}}, false
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return response{http.StatusServiceUnavailable, apiError{"pending", "outcome unknown: the order may still execute; resend with the same client_order_id to learn the result"}}, true
	}
	return engineResponse(err), false
}

func (s *Server) orderTarget(w http.ResponseWriter, r *http.Request) (*engine.Engine, uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_order_id", "order id must be an integer")
		return nil, 0, false
	}
	e, ok := s.x.ForOrder(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_order", "no such order")
		return nil, 0, false
	}
	return e, id, true
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	trader, ok := s.traderID(w, r)
	if !ok {
		return
	}
	e, id, ok := s.orderTarget(w, r)
	if !ok {
		return
	}
	s.submit(w, r, e, book.Command{Kind: book.CmdCancel, Trader: trader, OrderID: id})
}

type amendRequest struct {
	Price int64 `json:"price"`
	Qty   int64 `json:"qty"`
}

func (s *Server) handleAmend(w http.ResponseWriter, r *http.Request) {
	trader, ok := s.traderID(w, r)
	if !ok {
		return
	}
	e, id, ok := s.orderTarget(w, r)
	if !ok {
		return
	}
	var req amendRequest
	if !decodeBody(w, r, &req) {
		return
	}
	cmd := book.Command{Kind: book.CmdAmend, Trader: trader, OrderID: id, Price: req.Price, Qty: req.Qty}
	if book.Validate(cmd) == book.ReasonNone {
		ctx, cancel := context.WithTimeout(r.Context(), submitTimeout)
		rerr, err := s.checkAmend(ctx, e, cmd)
		cancel()
		if err != nil {
			engineError(w, err)
			return
		}
		if rerr != nil {
			writeRisk(w, rerr)
			return
		}
	}
	s.submit(w, r, e, cmd)
}

type openOrder struct {
	OrderID  uint64 `json:"order_id"`
	Side     string `json:"side"`
	Price    int64  `json:"price"`
	Qty      int64  `json:"qty"`
	Filled   int64  `json:"filled"`
	PostOnly bool   `json:"post_only"`
}

// handleOpenOrders lists the trader's resting orders from the book's
// per-trader index, so the query costs the engine time proportional to
// that trader's orders, not the whole book.
func (s *Server) handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	trader, ok := s.traderID(w, r)
	if !ok {
		return
	}
	e, ok := s.x.Engine(r.URL.Query().Get("symbol"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_symbol", "no such symbol")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), submitTimeout)
	defer cancel()
	out := []openOrder{}
	err := e.Query(ctx, func(b *book.Book) {
		b.TraderOrders(trader, func(o *book.Order) {
			out = append(out, openOrder{OrderID: o.ID, Side: o.Side.String(), Price: o.Price, Qty: o.Qty, Filled: o.Filled, PostOnly: o.PostOnly})
		})
	})
	if err != nil {
		engineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
