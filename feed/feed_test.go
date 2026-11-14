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
	f := New("X", 16)
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
	f := New("X", 16)
	startFeed(t, f, nil)
	f.Publish([]book.Event{accepted(1), delta(2, book.Buy, 100, 5)})
	waitSeq(t, f, 2)
	s, err := f.Subscribe(10, 10)
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
	f := New("X", 2)
	startFeed(t, f, nil)
	slow, _ := f.Subscribe(10, 10)
	fast, _ := f.Subscribe(10, 10)
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

func TestGapTriggersResync(t *testing.T) {
	f := New("X", 16)
	resyncs := 0
	startFeed(t, f, func(context.Context) (State, error) {
		if resyncs++; resyncs == 1 {
			return State{}, nil
		}
		return State{Seq: 10, Bids: []book.Level{{Price: 99, Qty: 4}}}, nil
	})
	f.Publish([]book.Event{delta(1, book.Buy, 100, 5)})
	waitSeq(t, f, 1)
	s, _ := f.Subscribe(10, 10)

	// Seq 2..10 never arrive, as if the engine overran the feed.
	f.Publish([]book.Event{delta(11, book.Sell, 105, 1)})
	waitSeq(t, f, 11)
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("subscriber kept after resync")
	}
	if s.Reason() != ReasonResync || resyncs != 2 {
		t.Fatalf("reason %v resyncs %d", s.Reason(), resyncs)
	}
	snap := f.Book(0)
	if snap.Seq != 11 || len(snap.Bids) != 1 || snap.Bids[0] != [2]int64{99, 4} || snap.Asks[0] != [2]int64{105, 1} {
		t.Fatalf("book after resync %+v", snap)
	}
	// A stale batch, already covered by the resync state, is ignored.
	f.Publish([]book.Event{delta(9, book.Buy, 1, 1)})
	f.Publish([]book.Event{delta(12, book.Buy, 99, 0)})
	waitSeq(t, f, 12)
	if snap := f.Book(0); len(snap.Bids) != 0 || resyncs != 2 || f.Stats().Resyncs != 1 {
		t.Fatalf("after stale batch: %+v, %d resyncs", snap, resyncs)
	}
}

func TestPublishNeverBlocks(t *testing.T) {
	f := New("X", 1)
	for i := range uint64(inboundBatches + 10) {
		f.Publish([]book.Event{delta(i+1, book.Buy, 100, 1)})
	}
	if f.Stats().Overruns != 10 {
		t.Fatalf("overruns %d, want 10", f.Stats().Overruns)
	}
}
