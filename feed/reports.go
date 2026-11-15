package feed

import (
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/BhargavaAtharva/tape/book"
)

// Report is one execution report on a trader's private stream: an
// acknowledgement, rejection, fill, amend or cancel of one of their orders.
// Fills are reported to both sides; Liquidity says which side this is.
type Report struct {
	Type      string `json:"type"`
	Symbol    string `json:"symbol"`
	Seq       uint64 `json:"seq"`
	Time      int64  `json:"time"`
	Kind      string `json:"kind"`
	OrderID   uint64 `json:"order_id"`
	Side      string `json:"side,omitempty"`
	Price     int64  `json:"price,omitempty"`
	Qty       int64  `json:"qty"`
	Remaining int64  `json:"remaining"`
	Liquidity string `json:"liquidity,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Reports fans execution reports out to per-trader subscribers across all
// symbols. Reports are not conflated: each one matters to its owner.
type Reports struct {
	buf     int
	dropped atomic.Uint64

	mu   sync.Mutex
	subs map[uint64]map[*Subscriber]struct{}
}

func NewReports(buf int) *Reports {
	if buf <= 0 {
		buf = 256
	}
	return &Reports{buf: buf, subs: make(map[uint64]map[*Subscriber]struct{})}
}

func (r *Reports) Subscribe(trader uint64) *Subscriber {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := newSubscriber(r.buf)
	if r.subs[trader] == nil {
		r.subs[trader] = make(map[*Subscriber]struct{})
	}
	r.subs[trader][s] = struct{}{}
	return s
}

func (r *Reports) Unsubscribe(trader uint64, s *Subscriber) { r.drop(trader, s, ReasonNone) }

func (r *Reports) drop(trader uint64, s *Subscriber, reason DropReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked(trader, s, reason)
}

func (r *Reports) dropLocked(trader uint64, s *Subscriber, reason DropReason) {
	set := r.subs[trader]
	if _, ok := set[s]; !ok {
		return
	}
	delete(set, s)
	if len(set) == 0 {
		delete(r.subs, trader)
	}
	s.reason = reason
	close(s.done)
	if reason == ReasonSlow {
		r.dropped.Add(1)
	}
}

func (r *Reports) Subscribers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, set := range r.subs {
		n += len(set)
	}
	return n
}

func opposite(s book.Side) book.Side {
	if s == book.Buy {
		return book.Sell
	}
	return book.Buy
}

func (r *Reports) route(symbol string, evs []book.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subs) == 0 {
		return
	}
	for i := range evs {
		e := &evs[i]
		if e.Kind == book.EvBookDelta {
			continue
		}
		rep := Report{Type: "exec", Symbol: symbol, Seq: e.Seq, Time: e.Time / 1e6, Kind: e.Kind.String(),
			OrderID: e.OrderID, Price: e.Price, Qty: e.Qty, Reason: e.Reason.String()}
		if e.Side != 0 {
			rep.Side = e.Side.String()
		}
		if e.Kind == book.EvFill {
			rep.Remaining, rep.Liquidity = e.Remaining, "taker"
			r.sendLocked(e.Trader, rep)
			rep.OrderID, rep.Side, rep.Remaining, rep.Liquidity = e.MakerOrderID, opposite(e.Side).String(), e.MakerRemaining, "maker"
			r.sendLocked(e.MakerTrader, rep)
			continue
		}
		if e.Kind == book.EvAccepted || e.Kind == book.EvAmended {
			rep.Remaining = e.Qty
		}
		r.sendLocked(e.Trader, rep)
	}
}

func (r *Reports) sendLocked(trader uint64, rep Report) {
	set := r.subs[trader]
	if len(set) == 0 {
		return
	}
	msg, err := json.Marshal(rep)
	if err != nil {
		slog.Error("report encode failed", "err", err)
		return
	}
	for s := range set {
		select {
		case s.ch <- msg:
		default:
			r.dropLocked(trader, s, ReasonSlow)
		}
	}
}
