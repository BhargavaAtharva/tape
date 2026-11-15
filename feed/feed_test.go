package feed

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BhargavaAtharva/tape/book"
)

func delta(seq uint64, s book.Side, price, qty int64) book.Event {
	return book.Event{Seq: seq, Kind: book.EvBookDelta, Side: s, Price: price, Qty: qty}
}

func fill(seq uint64, price, qty int64) book.Event {
	return book.Event{Seq: seq, Kind: book.EvFill, Side: book.Buy, OrderID: 9, MakerOrderID: 3, Price: price, Qty: qty, Time: 5e6}
}

func accepted(seq uint64) book.Event { return book.Event{Seq: seq, Kind: book.EvAccepted} }

func startFeed(t *testing.T, f *Feed, resync func(context.Context) (State, error)) {
	t.Helper()
	if resync == nil {
		calls := 0
		resync = func(context.Context) (State, error) {
			if calls++; calls > 1 {
				t.Error("unexpected resync")
			}
			return State{}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx, resync); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitSeq(t *testing.T, f *Feed, seq uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := f.seq
		f.mu.Unlock()
		if got >= seq {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed stuck at seq %d, want %d", got, seq)
		}
		time.Sleep(time.Millisecond)
	}
}

func receive[T any](t *testing.T, s *Subscriber) T {
	t.Helper()
	var v T
	select {
	case msg := <-s.C():
		if err := json.Unmarshal(msg, &v); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
	}
	return v
}

func TestMirrorTapeAndTicker(t *testing.T) {
	f := New("X", Options{Buffer: 16})
	startFeed(t, f, nil)
	f.Publish([]book.Event{accepted(1), delta(2, book.Buy, 100, 5), accepted(3), delta(4, book.Sell, 102, 7)})
	f.Publish([]book.Event{accepted(5), fill(6, 102, 3), delta(7, book.Sell, 102, 4), delta(8, book.Buy, 101, 2)})
	f.Publish([]book.Event{accepted(9)})
	waitSeq(t, f, 9)

	snap := f.Book(0)
	if snap.Seq != 8 || len(snap.Bids) != 2 || snap.Bids[0] != [2]int64{101, 2} || snap.Asks[0] != [2]int64{102, 4} {
		t.Fatalf("book %+v", snap)
	}
	tr := f.Trades(10)
	if len(tr) != 1 || tr[0].Price != 102 || tr[0].Qty != 3 || tr[0].Time != 5 || tr[0].TakerOrderID != 9 {
		t.Fatalf("trades %+v", tr)
	}
	tk := f.Ticker()
	if tk.Last != 102 || tk.Volume != 3 || tk.BestBid != 101 || tk.BestAsk != 102 {
		t.Fatalf("ticker %+v", tk)
	}
}

func TestSubscriberGetsSnapshotThenChainedUpdates(t *testing.T) {
	f := New("X", Options{Buffer: 16})
	startFeed(t, f, nil)
	f.Publish([]book.Event{accepted(1), delta(2, book.Buy, 100, 5)})
	waitSeq(t, f, 2)
	s, err := f.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	f.Publish([]book.Event{accepted(3)})
	f.Publish([]book.Event{accepted(4), delta(5, book.Buy, 100, 9), delta(6, book.Buy, 100, 8)})
	f.Publish([]book.Event{accepted(7), delta(8, book.Sell, 103, 1)})

	snap := receive[Snapshot](t, s)
	if snap.Type != "snapshot" || snap.Seq != 2 || snap.Bids[0] != [2]int64{100, 5} {
		t.Fatalf("snapshot %+v", snap)
	}
	u1 := receive[Update](t, s)
	if u1.Prev != 2 || u1.Seq != 6 || len(u1.Deltas) != 1 || u1.Deltas[0].Qty != 8 {
		t.Fatalf("first update %+v (deltas to one level should coalesce)", u1)
	}
	u2 := receive[Update](t, s)
	if u2.Prev != 6 || u2.Seq != 8 {
		t.Fatalf("second update %+v", u2)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	f := New("X", Options{Buffer: 2})
	startFeed(t, f, nil)
	slow, _ := f.Subscribe()
	fast, _ := f.Subscribe()
	got := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-fast.C():
				n++
			case <-fast.Done():
				got <- n
				return
			}
		}
	}()
	for i := range uint64(20) {
		f.Publish([]book.Event{delta(i+1, book.Buy, 100, int64(i+1))})
		waitSeq(t, f, i+1)
	}
	select {
	case <-slow.Done():
	case <-time.After(time.Second):
		t.Fatal("slow subscriber was not dropped")
	}
	if slow.Reason() != ReasonSlow || f.Stats().Dropped != 1 || f.Stats().Subscribers != 1 {
		t.Fatalf("reason %v stats %+v", slow.Reason(), f.Stats())
	}
	f.Unsubscribe(fast)
	if n := <-got; n != 21 {
		t.Fatalf("fast subscriber got %d messages, want 21", n)
	}
}

func TestGapTriggersResyncAndFreshSnapshot(t *testing.T) {
	f := New("X", Options{Buffer: 16, Epoch: "e1"})
	resyncs := 0
	startFeed(t, f, func(context.Context) (State, error) {
		if resyncs++; resyncs == 1 {
			return State{}, nil
		}
		return State{Seq: 10, Bids: []book.Level{{Price: 99, Qty: 4}}}, nil
	})
	f.Publish([]book.Event{delta(1, book.Buy, 100, 5)})
	waitSeq(t, f, 1)
	s, _ := f.Subscribe()
	if snap := receive[Snapshot](t, s); snap.Seq != 1 || snap.Epoch != "e1" {
		t.Fatalf("first snapshot %+v", snap)
	}

	// Seq 2..10 never arrive, as if the engine overran the feed.
	f.Publish([]book.Event{delta(11, book.Sell, 105, 1)})
	waitSeq(t, f, 11)
	snap := receive[Snapshot](t, s)
	if snap.Type != "snapshot" || snap.Seq != 10 || snap.Bids[0] != [2]int64{99, 4} || len(snap.Asks) != 0 {
		t.Fatalf("resync snapshot %+v", snap)
	}
	u := receive[Update](t, s)
	if u.Prev != 10 || u.Seq != 11 || u.Deltas[0].Price != 105 {
		t.Fatalf("update after resync %+v", u)
	}
	select {
	case <-s.Done():
		t.Fatal("subscriber dropped by a resync")
	default:
	}
	if resyncs != 2 || f.Stats().Resyncs != 1 {
		t.Fatalf("resyncs %d, stats %+v", resyncs, f.Stats())
	}
}

// The engine answers a resync query between two commands of a batch it has
// not published yet, so the batch that follows overlaps the resync state.
// The overlapping events must be skipped, not treated as another gap.
func TestBatchOverlappingResyncStateIsTrimmed(t *testing.T) {
	f := New("X", Options{Buffer: 16})
	calls := 0
	startFeed(t, f, func(context.Context) (State, error) {
		if calls++; calls == 1 {
			return State{}, nil
		}
		return State{Seq: 6, Bids: []book.Level{{Price: 100, Qty: 3}}}, nil
	})
	f.Publish([]book.Event{delta(1, book.Buy, 100, 1)})
	waitSeq(t, f, 1)
	f.Publish([]book.Event{delta(5, book.Buy, 100, 2), delta(6, book.Buy, 100, 3), delta(7, book.Buy, 100, 4)})
	waitSeq(t, f, 7)
	if calls != 2 {
		t.Fatalf("%d resync calls, want 2", calls)
	}
	if b := f.Book(0); b.Bids[0] != [2]int64{100, 4} {
		t.Fatalf("book %+v", b)
	}
}

func TestUpdatesAreConflated(t *testing.T) {
	f := New("X", Options{Buffer: 16, Interval: 50 * time.Millisecond})
	startFeed(t, f, nil)
	s, _ := f.Subscribe()
	receive[Snapshot](t, s)
	for i := range uint64(20) {
		f.Publish([]book.Event{delta(2*i+1, book.Buy, 100, int64(i+1)), fill(2*i+2, 100, 1)})
	}
	waitSeq(t, f, 40)
	u := receive[Update](t, s)
	if u.Prev != 0 || u.Seq != 40 || len(u.Deltas) != 1 || u.Deltas[0].Qty != 20 || len(u.Trades) != 20 {
		t.Fatalf("conflated update: prev %d seq %d, %d deltas, %d trades", u.Prev, u.Seq, len(u.Deltas), len(u.Trades))
	}
}

func TestPrivateReports(t *testing.T) {
	r := NewReports(8)
	f := New("X", Options{Buffer: 8, Reports: r})
	startFeed(t, f, nil)
	mine := r.Subscribe(7)
	other := r.Subscribe(8)
	f.Publish([]book.Event{
		{Seq: 1, Kind: book.EvAccepted, OrderID: 3, Trader: 7, Side: book.Sell, Price: 100, Qty: 5},
		{Seq: 2, Kind: book.EvBookDelta, Side: book.Sell, Price: 100, Qty: 5},
		{Seq: 3, Kind: book.EvAccepted, OrderID: 4, Trader: 9, Side: book.Buy, Price: 100, Qty: 2},
		{Seq: 4, Kind: book.EvFill, OrderID: 4, Trader: 9, Side: book.Buy, MakerOrderID: 3, MakerTrader: 7, Price: 100, Qty: 2, MakerRemaining: 3},
	})
	ack := receive[Report](t, mine)
	fillRep := receive[Report](t, mine)
	if ack.Kind != "accepted" || ack.OrderID != 3 || ack.Remaining != 5 {
		t.Fatalf("ack %+v", ack)
	}
	if fillRep.Kind != "fill" || fillRep.OrderID != 3 || fillRep.Side != "sell" || fillRep.Remaining != 3 || fillRep.Liquidity != "maker" {
		t.Fatalf("maker fill %+v", fillRep)
	}
	select {
	case m := <-other.C():
		t.Fatalf("another trader got %s", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishNeverBlocks(t *testing.T) {
	f := New("X", Options{Buffer: 1})
	for i := range uint64(inboundBatches + 10) {
		f.Publish([]book.Event{delta(i+1, book.Buy, 100, 1)})
	}
	if f.Stats().Overruns != 10 {
		t.Fatalf("overruns %d, want 10", f.Stats().Overruns)
	}
}
