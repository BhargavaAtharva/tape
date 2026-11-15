// Package feed turns a symbol's engine events into market data: an L2 book
// mirror, a trades tape and per-symbol statistics, fanned out to subscribers.
//
// The engine hands batches to Publish, which never blocks. A feed goroutine
// applies them in sequence order and, every Interval, sends subscribers one
// conflated update: the latest quantity of each level that changed and the
// trades since the last update. If the engine ever outruns the feed, the
// feed sees a sequence gap, rebuilds its mirror from the engine and sends
// every subscriber a fresh snapshot; the engine is never slowed down by the
// feed or by anyone subscribed to it.
package feed

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BhargavaAtharva/tape/book"
)

const (
	inboundBatches = 4096
	tapeLength     = 500
	snapshotTrades = 100
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

// Update covers every engine event after Prev up to and including Seq.
// Prev is the Seq of the message before it, so a subscriber that gets an
// Update whose Prev is not the last Seq it saw knows it has missed something.
type Update struct {
	Type   string  `json:"type"`
	Symbol string  `json:"symbol"`
	Seq    uint64  `json:"seq"`
	Prev   uint64  `json:"prev"`
	Deltas []Delta `json:"deltas"`
	Trades []Trade `json:"trades"`
}

// Snapshot is the whole book. Epoch identifies the server process: sequence
// numbers restart when the process does, and a client that sees a new epoch
// must drop everything it holds.
type Snapshot struct {
	Type   string     `json:"type"`
	Symbol string     `json:"symbol"`
	Epoch  string     `json:"epoch"`
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

type Options struct {
	// Buffer is how many messages a subscriber may fall behind by before it
	// is dropped.
	Buffer int
	// Interval is the conflation period. Zero sends one update per engine
	// batch.
	Interval time.Duration
	Epoch    string
	// Reports, if set, receives every applied batch for the private
	// per-trader execution streams.
	Reports *Reports
}

type pendingKey struct {
	side  book.Side
	price int64
}

type Feed struct {
	symbol string
	opts   Options
	in     chan []book.Event

	overruns atomic.Uint64
	resyncs  atomic.Uint64
	dropped  atomic.Uint64

	mu   sync.Mutex
	bids ladder
	asks ladder
	// seq is the last engine event applied to the mirror; prev the Seq of
	// the last message sent, which is where a new subscriber starts.
	seq          uint64
	prev         uint64
	pending      []Delta
	pendingIndex map[pendingKey]int
	pendingTrade []Trade
	tape         []Trade
	tapeAt       int
	ticker       Ticker
	subs         map[*Subscriber]struct{}
}

func New(symbol string, opts Options) *Feed {
	if opts.Buffer <= 0 {
		opts.Buffer = 256
	}
	return &Feed{
		symbol:       symbol,
		opts:         opts,
		in:           make(chan []book.Event, inboundBatches),
		bids:         newLadder(true),
		asks:         newLadder(false),
		pendingIndex: make(map[pendingKey]int),
		subs:         make(map[*Subscriber]struct{}),
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
	var tick <-chan time.Time
	if f.opts.Interval > 0 {
		t := time.NewTicker(f.opts.Interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			f.closeAll()
			return
		case <-tick:
			f.mu.Lock()
			f.flushLocked()
			f.mu.Unlock()
		case evs := <-f.in:
			f.mu.Lock()
			f.ingestLocked(ctx, evs, resync)
			if tick == nil {
				f.flushLocked()
			}
			f.mu.Unlock()
		}
	}
}

// ingestLocked applies the events of a batch that the mirror has not seen.
// A batch can overlap the mirror after a resync, because the engine answers
// a resync query between two commands of a batch it has not published yet;
// the overlapping events are already reflected in the state and are skipped.
func (f *Feed) ingestLocked(ctx context.Context, evs []book.Event, resync func(context.Context) (State, error)) {
	for len(evs) > 0 && evs[0].Seq <= f.seq {
		evs = evs[1:]
	}
	if len(evs) == 0 {
		return
	}
	if evs[0].Seq != f.seq+1 {
		if err := f.resyncLocked(ctx, resync); err != nil {
			if ctx.Err() == nil {
				slog.Error("feed resync failed", "symbol", f.symbol, "err", err)
			}
			return
		}
		for len(evs) > 0 && evs[0].Seq <= f.seq {
			evs = evs[1:]
		}
		if len(evs) == 0 {
			return
		}
	}
	f.applyLocked(evs)
	if f.opts.Reports != nil {
		f.opts.Reports.route(f.symbol, evs)
	}
}

// resyncLocked replaces the mirror with the engine's state and sends every
// subscriber a fresh snapshot in place of the deltas that were lost.
func (f *Feed) resyncLocked(ctx context.Context, resync func(context.Context) (State, error)) error {
	st, err := resync(ctx)
	if err != nil {
		return err
	}
	if st.Seq == f.seq {
		return nil
	}
	if f.seq != 0 || len(f.subs) > 0 {
		slog.Warn("feed resynced", "symbol", f.symbol, "from_seq", f.seq, "to_seq", st.Seq, "subscribers", len(f.subs))
		f.resyncs.Add(1)
	}
	f.bids.reset(st.Bids)
	f.asks.reset(st.Asks)
	f.pending = f.pending[:0]
	clear(f.pendingIndex)
	f.pendingTrade = f.pendingTrade[:0]
	f.seq, f.prev = st.Seq, st.Seq
	f.updateBestLocked()
	if len(f.subs) == 0 {
		return nil
	}
	msg, err := json.Marshal(f.snapshotLocked(0, snapshotTrades))
	if err != nil {
		return err
	}
	f.broadcastLocked(msg)
	return nil
}

func sideName(s book.Side) string {
	if s == book.Buy {
		return "buy"
	}
	return "sell"
}

func (f *Feed) applyLocked(evs []book.Event) {
	for i := range evs {
		e := &evs[i]
		switch e.Kind {
		case book.EvBookDelta:
			if e.Side == book.Buy {
				f.bids.set(e.Price, e.Qty)
			} else {
				f.asks.set(e.Price, e.Qty)
			}
			k := pendingKey{e.Side, e.Price}
			if j, ok := f.pendingIndex[k]; ok {
				f.pending[j].Qty = e.Qty
			} else {
				f.pendingIndex[k] = len(f.pending)
				f.pending = append(f.pending, Delta{Side: sideName(e.Side), Price: e.Price, Qty: e.Qty})
			}
		case book.EvFill:
			t := Trade{Seq: e.Seq, Time: e.Time / 1e6, Price: e.Price, Qty: e.Qty, Side: sideName(e.Side),
				MakerOrderID: e.MakerOrderID, TakerOrderID: e.OrderID}
			f.pendingTrade = append(f.pendingTrade, t)
			f.recordTradeLocked(t)
		}
	}
	f.seq = evs[len(evs)-1].Seq
	f.updateBestLocked()
}

// flushLocked sends what has accumulated since the last update as one
// message. Batches with nothing for market data advance seq without a
// message; the next update's range simply covers them.
func (f *Feed) flushLocked() {
	if len(f.pending) == 0 && len(f.pendingTrade) == 0 {
		return
	}
	u := Update{Type: "update", Symbol: f.symbol, Prev: f.prev, Seq: f.seq, Deltas: f.pending, Trades: f.pendingTrade}
	var msg []byte
	var err error
	if len(f.subs) > 0 {
		msg, err = json.Marshal(u)
	}
	f.prev = f.seq
	f.pending = f.pending[:0]
	clear(f.pendingIndex)
	f.pendingTrade = f.pendingTrade[:0]
	if err != nil {
		slog.Error("feed encode failed", "symbol", f.symbol, "err", err)
		return
	}
	f.broadcastLocked(msg)
}

func (f *Feed) broadcastLocked(msg []byte) {
	for s := range f.subs {
		select {
		case s.ch <- msg:
		default:
			f.dropLocked(s, ReasonSlow)
		}
	}
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
	f.ticker.BestBid, _ = f.bids.best()
	f.ticker.BestAsk, _ = f.asks.best()
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

// snapshotLocked describes the book as of f.prev, so callers must flush
// first unless nothing is pending.
func (f *Feed) snapshotLocked(depth, trades int) Snapshot {
	snap := Snapshot{
		Type: "snapshot", Symbol: f.symbol, Epoch: f.opts.Epoch, Seq: f.prev,
		Bids: f.bids.top(depth),
		Asks: f.asks.top(depth),
	}
	if trades > 0 {
		snap.Trades = f.tradesLocked(trades)
	}
	return snap
}

// Book returns the current L2 view, up to depth levels a side (0 = all).
func (f *Feed) Book(depth int) Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushLocked()
	return f.snapshotLocked(depth, 0)
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
	ReasonShutdown
)

func (r DropReason) String() string {
	switch r {
	case ReasonSlow:
		return "slow consumer"
	case ReasonShutdown:
		return "shutting down"
	}
	return ""
}

// Subscriber receives encoded messages on C until Done is closed. The first
// message is always a snapshot, and so is any message after a resync; all
// others are updates.
type Subscriber struct {
	ch     chan []byte
	done   chan struct{}
	reason DropReason
}

func (s *Subscriber) C() <-chan []byte { return s.ch }

// Done is closed when the subscriber is dropped; Reason then says why.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

func (s *Subscriber) Reason() DropReason {
	<-s.done
	return s.reason
}

func newSubscriber(buf int) *Subscriber {
	return &Subscriber{ch: make(chan []byte, buf), done: make(chan struct{})}
}

// Subscribe registers a subscriber and queues a snapshot of the full book
// for it. Pending changes are flushed first and everything happens under
// the lock that guards updates, so the subscriber's first update follows
// its snapshot exactly. Snapshots are always full depth: a client given a
// partial book could not apply deltas below its cut-off without holes.
func (f *Feed) Subscribe() (*Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushLocked()
	msg, err := json.Marshal(f.snapshotLocked(0, snapshotTrades))
	if err != nil {
		return nil, err
	}
	s := newSubscriber(f.opts.Buffer)
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
