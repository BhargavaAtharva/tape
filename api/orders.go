package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
)

// Trader ids must survive a round trip through a JavaScript number.
const maxTrader = 1<<53 - 1

const submitTimeout = 5 * time.Second

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
	OrderID   uint64      `json:"order_id"`
	Symbol    string      `json:"symbol"`
	Status    string      `json:"status"`
	Reason    string      `json:"reason,omitempty"`
	Filled    int64       `json:"filled"`
	Remaining int64       `json:"remaining"`
	Events    []eventJSON `json:"events"`
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

func traderID(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	id, err := strconv.ParseUint(r.Header.Get("X-Trader-ID"), 10, 64)
	if err != nil || id == 0 || id > maxTrader {
		writeError(w, http.StatusUnauthorized, "invalid_trader", "X-Trader-ID must be an integer between 1 and 2^53-1")
		return 0, false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
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
	trader, ok := traderID(w, r)
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
	cmd, msg := req.command(trader)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_order", msg)
		return
	}
	s.submit(w, r, e, cmd)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request, e *engine.Engine, cmd book.Command) {
	ctx, cancel := context.WithTimeout(r.Context(), submitTimeout)
	defer cancel()
	res, err := e.Submit(ctx, cmd)
	if err != nil {
		engineError(w, err)
		return
	}
	out, reason := summarise(e.Symbol(), cmd.OrderID, res.Events)
	writeJSON(w, rejectStatus(reason), out)
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
	trader, ok := traderID(w, r)
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
	trader, ok := traderID(w, r)
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
	s.submit(w, r, e, book.Command{Kind: book.CmdAmend, Trader: trader, OrderID: id, Price: req.Price, Qty: req.Qty})
}

type openOrder struct {
	OrderID  uint64 `json:"order_id"`
	Side     string `json:"side"`
	Price    int64  `json:"price"`
	Qty      int64  `json:"qty"`
	PostOnly bool   `json:"post_only"`
}

// handleOpenOrders scans the book on the engine goroutine. The book keeps
// no per-trader index because matching never needs one; a linear scan is
// fine for a status query.
func (s *Server) handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	trader, ok := traderID(w, r)
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
		collect := func(o *book.Order) {
			if o.Trader == trader {
				out = append(out, openOrder{OrderID: o.ID, Side: o.Side.String(), Price: o.Price, Qty: o.Qty, PostOnly: o.PostOnly})
			}
		}
		b.Orders(book.Buy, collect)
		b.Orders(book.Sell, collect)
	})
	if err != nil {
		engineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
