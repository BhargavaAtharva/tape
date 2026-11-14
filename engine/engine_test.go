package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BhargavaAtharva/tape/book"
)

func limit(trader uint64, s book.Side, price, qty int64) book.Command {
	return book.Command{Kind: book.CmdNew, Type: book.Limit, TIF: book.GTC, Trader: trader, Side: s, Price: price, Qty: qty}
}

// collector is an event sink that also verifies sequence continuity.
type collector struct {
	mu     sync.Mutex
	events []book.Event
	gap    bool
}

func (c *collector) add(evs []book.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range evs {
		if e.Seq != uint64(len(c.events))+1 {
			c.gap = true
		}
		c.events = append(c.events, e)
	}
}

func (c *collector) snapshot() ([]book.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]book.Event(nil), c.events...), c.gap
}

func startEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	t.Cleanup(func() { e.Close() })
	return e
}

func TestSubmitReturnsTheCommandsEvents(t *testing.T) {
	e := startEngine(t, Config{Symbol: "X"})
	ctx := context.Background()

	res, err := e.Submit(ctx, limit(1, book.Sell, 100, 5))
	if err != nil {
		t.Fatal(err)
	}
	if res.Events[0].Kind != book.EvAccepted || res.Events[0].OrderID != 1 {
		t.Fatalf("first result %+v", res.Events)
	}
	res, err = e.Submit(ctx, limit(2, book.Buy, 100, 5))
	if err != nil {
		t.Fatal(err)
	}
	var fills int
	for _, ev := range res.Events {
		if ev.Kind == book.EvFill {
			fills++
			if ev.MakerOrderID != 1 || ev.Qty != 5 {
				t.Fatalf("fill %+v", ev)
			}
		}
	}
	if fills != 1 {
		t.Fatalf("%d fills, want 1", fills)
	}
	if st := e.Stats(); st.Commands != 2 || st.Fills != 1 || st.Seq != e.book.Seq() {
		t.Fatalf("stats %+v", st)
	}
}

func TestConcurrentSubmitters(t *testing.T) {
	const workers, perWorker = 16, 500
	var sink collector
	e := startEngine(t, Config{Symbol: "X", Events: sink.add, QueueSize: 64, MaxBatch: 32})
	ctx := context.Background()

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trader := uint64(w + 1)
			var mine []uint64
			for i := range perWorker {
				side := book.Buy
				if (w+i)%2 == 0 {
					side = book.Sell
				}
				res, err := e.Submit(ctx, limit(trader, side, int64(95+(i*7+w)%11), int64(1+i%9)))
				if err != nil {
					t.Error(err)
					return
				}
				if res.Events[0].Kind != book.EvAccepted || res.Events[0].Trader != trader {
					t.Errorf("worker %d got someone else's result: %+v", w, res.Events[0])
					return
				}
				accepted.Add(1)
				mine = append(mine, res.Events[0].OrderID)
				if i%5 == 4 {
					id := mine[i/2]
					if _, err := e.Submit(ctx, book.Command{Kind: book.CmdCancel, Trader: trader, OrderID: id}); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != workers*perWorker {
		t.Fatalf("accepted %d of %d", accepted.Load(), workers*perWorker)
	}

	var seq uint64
	var resting int
	if err := e.Query(ctx, func(b *book.Book) { seq, resting = b.Seq(), b.Len() }); err != nil {
		t.Fatal(err)
	}
	events, gap := sink.snapshot()
	if gap {
		t.Fatal("sink saw a sequence gap")
	}
	if uint64(len(events)) != seq {
		t.Fatalf("sink has %d events, book seq is %d", len(events), seq)
	}

	// Rebuild open quantity per order from the stream and compare with the book.
	open := make(map[uint64]int64)
	for _, ev := range events {
		switch ev.Kind {
		case book.EvAccepted:
			open[ev.OrderID] = ev.Qty
		case book.EvFill:
			open[ev.OrderID] -= ev.Qty
			open[ev.MakerOrderID] -= ev.Qty
		case book.EvCancelled:
			open[ev.OrderID] -= ev.Qty
		}
	}
	live := 0
	for id, q := range open {
		if q < 0 {
			t.Fatalf("order %d overfilled by %d", id, -q)
		}
		if q > 0 {
			live++
		}
	}
	if live != resting {
		t.Fatalf("events imply %d resting orders, book has %d", live, resting)
	}
}

func TestExchangeRoutesBySymbolAndOrderID(t *testing.T) {
	symbols := []string{"AAA", "BBB", "CCC"}
	sinks := make(map[string]*collector)
	x, err := OpenExchange(symbols, func(c *Config) {
		s := &collector{}
		sinks[c.Symbol] = s
		c.Events = s.add
	})
	if err != nil {
		t.Fatal(err)
	}
	x.Start()
	defer x.Close()
	ctx := context.Background()

	const perSymbol = 300
	var wg sync.WaitGroup
	ids := make([][]uint64, len(symbols))
	for i, s := range symbols {
		e, ok := x.Engine(s)
		if !ok {
			t.Fatalf("no engine for %s", s)
		}
		for w := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range perSymbol / 4 {
					res, err := e.Submit(ctx, limit(uint64(w+1), book.Buy, int64(100-w), int64(n+1)))
					if err != nil {
						t.Error(err)
						return
					}
					if w == 0 {
						ids[i] = append(ids[i], res.Events[0].OrderID)
					}
				}
			}()
		}
	}
	wg.Wait()

	for i, s := range symbols {
		e, _ := x.Engine(s)
		for _, id := range ids[i] {
			got, ok := x.ForOrder(id)
			if !ok || got != e {
				t.Fatalf("order %d of %s routed to the wrong engine", id, s)
			}
		}
		events, gap := sinks[s].snapshot()
		if gap {
			t.Fatalf("%s: sequence gap", s)
		}
		accepted := 0
		for _, ev := range events {
			if ev.Kind == book.EvAccepted {
				accepted++
			}
		}
		if accepted != perSymbol {
			t.Fatalf("%s: %d orders accepted, want %d", s, accepted, perSymbol)
		}
	}
	if _, ok := x.Engine("ZZZ"); ok {
		t.Fatal("unknown symbol resolved")
	}
	for _, id := range []uint64{0, 5, uint64(len(symbols)+1) << idShift} {
		if _, ok := x.ForOrder(id); ok {
			t.Fatalf("order id %d resolved to an engine", id)
		}
	}
}

func TestOpenExchangeRejectsBadSymbolLists(t *testing.T) {
	for _, symbols := range [][]string{nil, {"A", "A"}, {"A", ""}, {"../X"}, {"btc"}, {".A"}, {"ABCDEFGHIJKLMNOPQ"}} {
		if _, err := OpenExchange(symbols, nil); err == nil {
			t.Fatalf("symbols %q accepted", symbols)
		}
	}
}

func TestCloseDrainsQueuedCommands(t *testing.T) {
	var sink collector
	e, err := Open(Config{Symbol: "X", Events: sink.add, QueueSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range 100 {
		if err := e.Enqueue(ctx, limit(1, book.Buy, 100, int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	e.Start()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e.Book().Len() != 100 {
		t.Fatalf("%d of 100 queued commands applied before close", e.Book().Len())
	}
	if _, err := e.Submit(ctx, limit(1, book.Buy, 100, 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after close: %v", err)
	}
	if err := e.Query(ctx, func(*book.Book) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("query after close: %v", err)
	}
}

func TestSubmitHonoursContextWhenQueueIsFull(t *testing.T) {
	e, err := Open(Config{Symbol: "X", QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Enqueue(context.Background(), limit(1, book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.Submit(ctx, limit(1, book.Buy, 100, 1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("submit on a full queue: %v", err)
	}
}

func TestClockStampsEvents(t *testing.T) {
	var tick int64
	e := startEngine(t, Config{Symbol: "X", Clock: func() int64 { tick++; return tick }})
	for want := int64(1); want <= 3; want++ {
		res, err := e.Submit(context.Background(), limit(1, book.Buy, 100, 1))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range res.Events {
			if ev.Time != want {
				t.Fatalf("event time %d, want %d", ev.Time, want)
			}
		}
	}
}

func TestEngineExpiresOrdersFromItsTimer(t *testing.T) {
	var now atomic.Int64
	var sink collector
	e := startEngine(t, Config{Symbol: "X", Events: sink.add, ExpiryCheck: time.Millisecond, Clock: func() int64 { return now.Load() }})
	c := limit(1, book.Buy, 100, 5)
	c.TTL = 1000
	if _, err := e.Submit(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if events, _ := sink.snapshot(); len(events) != 2 {
		t.Fatalf("%d events before the order was due", len(events))
	}
	now.Store(1000)
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, gap := sink.snapshot()
		if gap {
			t.Fatal("sequence gap")
		}
		if last := events[len(events)-1]; last.Kind == book.EvBookDelta && len(events) == 4 {
			if c := events[2]; c.Kind != book.EvCancelled || c.Reason != book.ReasonExpired || c.Time != 1000 {
				t.Fatalf("expiry event %+v", c)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("order not expired: %+v", events)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStaticallyInvalidCommandsAreNotLogged(t *testing.T) {
	var sink collector
	e := startEngine(t, Config{Symbol: "X", Events: sink.add, Dir: t.TempDir()})
	res, err := e.Submit(context.Background(), book.Command{Kind: book.CmdNew, Trader: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100})
	if err != nil {
		t.Fatal(err)
	}
	if ev := res.Events[0]; ev.Kind != book.EvRejected || ev.Reason != book.ReasonInvalidQty || ev.Seq != 0 {
		t.Fatalf("rejection %+v", ev)
	}
	if _, err := e.Submit(context.Background(), limit(1, book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	if st := e.Stats(); st.WALWritten != 1 || st.Seq != 2 {
		t.Fatalf("stats %+v: the invalid command reached the log", st)
	}
}

func TestTrySubmitFailsFastWhenTheQueueIsFull(t *testing.T) {
	e, err := Open(Config{Symbol: "X", QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Enqueue(context.Background(), limit(1, book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TrySubmit(context.Background(), limit(1, book.Buy, 100, 1)); !errors.Is(err, ErrBusy) {
		t.Fatalf("TrySubmit on a full queue: %v", err)
	}
}
