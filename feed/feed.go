// Package feed turns a symbol's engine events into market data: an L2 book
// mirror, a trades tape and per-symbol statistics, fanned out to subscribers.
//
// The engine hands batches to Publish, which never blocks. A feed goroutine
// applies them in sequence order. If the engine ever outruns it, the feed
// sees a sequence gap, rebuilds its mirror from the engine and carries on;
// the engine is never slowed down by the feed or by anyone subscribed to it.
package feed

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/BhargavaAtharva/tape/book"
)

const (
	inboundBatches = 4096
	tapeLength     = 500
)

// State is what a feed needs to rebuild itself: the full L2 book as of the
// event with sequence number Seq.
type State struct {
	Seq  uint64
	Bids []book.Level
	Asks []book.Level
}

type Trade struct {
	Seq          uint64 `json:"seq"`
	Time         int64  `json:"time"`
	Price        int64  `json:"price"`
	Qty          int64  `json:"qty"`
	Side         string `json:"side"`
	MakerOrderID uint64 `json:"maker_order_id"`
	TakerOrderID uint64 `json:"taker_order_id"`
}

type Delta struct {
	Side  string `json:"side"`
	Price int64  `json:"price"`
	Qty   int64  `json:"qty"`
}

// Update is one engine batch as seen by market data. Prev is the Seq of the
// message before it, so a subscriber that gets an Update whose Prev is not
// the last Seq it saw knows it has missed something.
type Update struct {
	Type   string  `json:"type"`
	Symbol string  `json:"symbol"`
	Seq    uint64  `json:"seq"`
	Prev   uint64  `json:"prev"`
	Deltas []Delta `json:"deltas"`
	Trades []Trade `json:"trades"`
}

type Snapshot struct {
	Type   string     `json:"type"`
	Symbol string     `json:"symbol"`
	Seq    uint64     `json:"seq"`
	Bids   [][2]int64 `json:"bids"`
	Asks   [][2]int64 `json:"asks"`
	Trades []Trade    `json:"trades,omitempty"`
}

// Ticker summarises trading since the process started.
type Ticker struct {
	Last    int64 `json:"last"`
	Open    int64 `json:"open"`
	High    int64 `json:"high"`
	Low     int64 `json:"low"`
	Volume  int64 `json:"volume"`
	Trades  int64 `json:"trades"`
	BestBid int64 `json:"best_bid"`
	BestAsk int64 `json:"best_ask"`
}

type Feed struct {
	symbol string
	in     chan []book.Event
	bufLen int

	overruns atomic.Uint64
	resyncs  atomic.Uint64
	dropped  atomic.Uint64

	mu     sync.Mutex
	bids   map[int64]int64
	asks   map[int64]int64
	seq    uint64
	prev   uint64
	tape   []Trade
	tapeAt int
	ticker Ticker
	subs   map[*Subscriber]struct{}
}

// New creates a feed. bufLen is how many messages a subscriber may fall
// behind by before it is dropped.
func New(symbol string, bufLen int) *Feed {
	if bufLen <= 0 {
		bufLen = 256
	}
	return &Feed{
		symbol: symbol,
		in:     make(chan []book.Event, inboundBatches),
		bufLen: bufLen,
		bids:   make(map[int64]int64),
		asks:   make(map[int64]int64),
		subs:   make(map[*Subscriber]struct{}),
	}
}

func (f *Feed) Symbol() string { return f.symbol }

// Publish is the engine's event sink. It runs on the engine goroutine and
// never blocks: if the feed is too far behind, the batch is dropped and the
// feed resyncs when it notices the gap.
func (f *Feed) Publish(evs []book.Event) {
	select {
	case f.in <- evs:
	default:
		f.overruns.Add(1)
	}
}

// Run applies published batches until ctx is done. resync must return the
// engine's current L2 state. It is called once at the start, because an
// engine restored from a snapshot has state but no events to show for it,
// and again whenever a gap is detected.
func (f *Feed) Run(ctx context.Context, resync func(context.Context) (State, error)) {
	f.mu.Lock()
	if err := f.resyncLocked(ctx, resync); err != nil && ctx.Err() == nil {
		slog.Error("feed initial sync failed", "symbol", f.symbol, "err", err)
	}
	f.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			f.closeAll()
			return
		case evs := <-f.in:
			if len(evs) == 0 {
				continue
			}
			f.mu.Lock()
			if evs[len(evs)-1].Seq <= f.seq {
				f.mu.Unlock()
				continue
			}
			if evs[0].Seq != f.seq+1 {
				if err := f.resyncLocked(ctx, resync); err != nil {
					f.mu.Unlock()
					if ctx.Err() == nil {
						slog.Error("feed resync failed", "symbol", f.symbol, "err", err)
					}
					continue
				}
				if evs[len(evs)-1].Seq <= f.seq {
					f.mu.Unlock()
					continue
				}
			}
			f.applyLocked(evs)
			f.mu.Unlock()
		}
	}
}

// resyncLocked replaces the mirror with the engine's state. Subscribers are
// disconnected because the deltas they would need no longer exist; clients
// reconnect and start from a fresh snapshot.
func (f *Feed) resyncLocked(ctx context.Context, resync func(context.Context) (State, error)) error {
	st, err := resync(ctx)
	if err != nil {
		return err
	}
	if st.Seq == f.seq {
		return nil
	}
	clear(f.bids)
	clear(f.asks)
	for _, l := range st.Bids {
		f.bids[l.Price] = l.Qty
	}
	for _, l := range st.Asks {
		f.asks[l.Price] = l.Qty
	}
	if f.seq != 0 || len(f.subs) > 0 {
		slog.Warn("feed resynced", "symbol", f.symbol, "from_seq", f.seq, "to_seq", st.Seq, "subscribers_dropped", len(f.subs))
		f.resyncs.Add(1)
	}
	f.seq, f.prev = st.Seq, st.Seq
	for s := range f.subs {
		f.dropLocked(s, ReasonResync)
	}
	f.updateBestLocked()
	return nil
}

func sideName(s book.Side) string {
	if s == book.Buy {
		return "buy"
	}
	return "sell"
}

func (f *Feed) applyLocked(evs []book.Event) {
	u := Update{Type: "update", Symbol: f.symbol, Prev: f.prev, Seq: evs[len(evs)-1].Seq}
	for i := range evs {
		e := &evs[i]
		switch e.Kind {
		case book.EvBookDelta:
			levels := f.asks
			if e.Side == book.Buy {
				levels = f.bids
			}
			if e.Qty == 0 {
				delete(levels, e.Price)
			} else {
				levels[e.Price] = e.Qty
			}
			u.Deltas = appendDelta(u.Deltas, Delta{Side: sideName(e.Side), Price: e.Price, Qty: e.Qty})
		case book.EvFill:
			t := Trade{Seq: e.Seq, Time: e.Time / 1e6, Price: e.Price, Qty: e.Qty, Side: sideName(e.Side),
				MakerOrderID: e.MakerOrderID, TakerOrderID: e.OrderID}
			u.Trades = append(u.Trades, t)
			f.recordTradeLocked(t)
		}
	}
	f.seq = u.Seq
	if len(u.Deltas) == 0 && len(u.Trades) == 0 {
		return
	}
	f.updateBestLocked()
	f.prev = u.Seq
	if len(f.subs) == 0 {
		return
	}
	msg, err := json.Marshal(u)
	if err != nil {
		slog.Error("feed encode failed", "symbol", f.symbol, "err", err)
		return
	}
	for s := range f.subs {
		select {
		case s.ch <- msg:
		default:
			f.dropLocked(s, ReasonSlow)
		}
	}
}

// appendDelta keeps only the last value per level within one update, in the
// order levels were first touched.
func appendDelta(ds []Delta, d Delta) []Delta {
	for i := range ds {
		if ds[i].Side == d.Side && ds[i].Price == d.Price {
			ds[i].Qty = d.Qty
			return ds
		}
	}
	return append(ds, d)
}

func (f *Feed) recordTradeLocked(t Trade) {
	if len(f.tape) < tapeLength {
		f.tape = append(f.tape, t)
	} else {
		f.tape[f.tapeAt] = t
		f.tapeAt = (f.tapeAt + 1) % tapeLength
	}
	tk := &f.ticker
	if tk.Trades == 0 {
		tk.Open, tk.High, tk.Low = t.Price, t.Price, t.Price
	}
	tk.Last = t.Price
	tk.High = max(tk.High, t.Price)
	tk.Low = min(tk.Low, t.Price)
	tk.Volume += t.Qty
	tk.Trades++
}

func (f *Feed) updateBestLocked() {
	f.ticker.BestBid, f.ticker.BestAsk = 0, 0
	for p := range f.bids {
		f.ticker.BestBid = max(f.ticker.BestBid, p)
	}
	for p := range f.asks {
		if f.ticker.BestAsk == 0 || p < f.ticker.BestAsk {
			f.ticker.BestAsk = p
		}
	}
}

// tradesLocked returns up to n recent trades, oldest first.
func (f *Feed) tradesLocked(n int) []Trade {
	ordered := make([]Trade, 0, len(f.tape))
	ordered = append(append(ordered, f.tape[f.tapeAt:]...), f.tape[:f.tapeAt]...)
	if n > 0 && n < len(ordered) {
		ordered = ordered[len(ordered)-n:]
	}
	return ordered
}

func levels(m map[int64]int64, desc bool, depth int) [][2]int64 {
	out := make([][2]int64, 0, len(m))
	for p, q := range m {
		out = append(out, [2]int64{p, q})
	}
	slices.SortFunc(out, func(a, b [2]int64) int {
		if desc {
			return int(b[0] - a[0])
		}
		return int(a[0] - b[0])
	})
	if depth > 0 && depth < len(out) {
		out = out[:depth]
	}
	return out
}

func (f *Feed) snapshotLocked(depth, trades int) Snapshot {
	return Snapshot{
		Type: "snapshot", Symbol: f.symbol, Seq: f.prev,
		Bids:   levels(f.bids, true, depth),
		Asks:   levels(f.asks, false, depth),
		Trades: f.tradesLocked(trades),
	}
}

// Book returns the current L2 view, up to depth levels a side (0 = all).
func (f *Feed) Book(depth int) Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := f.snapshotLocked(depth, 0)
	snap.Trades = nil
	return snap
}

// Trades returns up to n recent trades, oldest first.
func (f *Feed) Trades(n int) []Trade {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tradesLocked(n)
}

func (f *Feed) Ticker() Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ticker
}

type Stats struct {
	Subscribers int    `json:"subscribers"`
	Dropped     uint64 `json:"dropped"`
	Overruns    uint64 `json:"overruns"`
	Resyncs     uint64 `json:"resyncs"`
}

func (f *Feed) Stats() Stats {
	f.mu.Lock()
	n := len(f.subs)
	f.mu.Unlock()
	return Stats{Subscribers: n, Dropped: f.dropped.Load(), Overruns: f.overruns.Load(), Resyncs: f.resyncs.Load()}
}

type DropReason int

const (
	ReasonNone DropReason = iota
	ReasonSlow
	ReasonResync
	ReasonShutdown
)

func (r DropReason) String() string {
	switch r {
	case ReasonSlow:
		return "slow consumer"
	case ReasonResync:
		return "feed resync"
	case ReasonShutdown:
		return "shutting down"
	}
	return ""
}

// Subscriber receives encoded messages on C until Done is closed. The first
// message is always a snapshot; every later one is an Update.
type Subscriber struct {
	ch     chan []byte
	done   chan struct{}
	reason DropReason
}

func (s *Subscriber) C() <-chan []byte { return s.ch }

// Done is closed when the feed drops the subscriber; Reason then says why.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

func (s *Subscriber) Reason() DropReason {
	<-s.done
	return s.reason
}

// Subscribe registers a subscriber and queues its initial snapshot. Taking
// the snapshot and registering happen under the same lock as applying
// updates, so the subscriber's first Update follows its snapshot exactly.
func (f *Feed) Subscribe(depth, trades int) (*Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, err := json.Marshal(f.snapshotLocked(depth, trades))
	if err != nil {
		return nil, err
	}
	s := &Subscriber{ch: make(chan []byte, f.bufLen), done: make(chan struct{})}
	s.ch <- msg
	f.subs[s] = struct{}{}
	return s, nil
}

// Unsubscribe removes a subscriber that is going away on its own.
func (f *Feed) Unsubscribe(s *Subscriber) { f.Drop(s, ReasonNone) }

// Drop removes a subscriber for the given reason. It is how the gateway
// reports a client it had to cut off because a write timed out.
func (f *Feed) Drop(s *Subscriber, r DropReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subs[s]; ok {
		f.dropLocked(s, r)
	}
}

func (f *Feed) dropLocked(s *Subscriber, r DropReason) {
	delete(f.subs, s)
	s.reason = r
	close(s.done)
	if r == ReasonSlow {
		f.dropped.Add(1)
	}
}

func (f *Feed) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.subs {
		f.dropLocked(s, ReasonShutdown)
	}
}
