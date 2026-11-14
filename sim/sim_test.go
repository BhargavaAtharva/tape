package sim

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
)

func TestBotsMakeAMarket(t *testing.T) {
	var fills, commands atomic.Int64
	x, err := engine.OpenExchange([]string{"AAA", "BBB"}, func(c *engine.Config) {
		c.Events = func(evs []book.Event) {
			for _, e := range evs {
				if e.Kind == book.EvFill {
					fills.Add(1)
				}
				if e.Kind == book.EvAccepted || e.Kind == book.EvRejected {
					commands.Add(1)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	x.Start()
	defer x.Close()

	cfg := Config{Markets: []Market{{Symbol: "AAA", Ref: 6_500_000}, {Symbol: "BBB", Ref: 250}}, Intensity: 10, Seed: 1}
	runFor := func(d time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		if err := Run(ctx, x, cfg); err != nil {
			t.Fatal(err)
		}
	}
	runFor(1500 * time.Millisecond)
	if fills.Load() == 0 || commands.Load() < 100 {
		t.Fatalf("%d commands and %d fills in 1.5s", commands.Load(), fills.Load())
	}
	for _, sym := range []string{"AAA", "BBB"} {
		e, _ := x.Engine(sym)
		var bids, asks []book.Level
		e.Query(context.Background(), func(b *book.Book) {
			bids, asks = b.Depth(book.Buy, 0), b.Depth(book.Sell, 0)
		})
		if len(bids) < makerLevels || len(asks) < makerLevels {
			t.Fatalf("%s: %d bid and %d ask levels", sym, len(bids), len(asks))
		}
	}

	// A second run, as after a restart, cancels the first run's quotes
	// before building its own, so no maker ends up with two ladders.
	runFor(300 * time.Millisecond)
	e, _ := x.Engine("AAA")
	perMaker := map[uint64]int{}
	e.Query(context.Background(), func(b *book.Book) {
		for _, s := range []book.Side{book.Buy, book.Sell} {
			b.Orders(s, func(o *book.Order) {
				if o.Trader <= makers {
					perMaker[o.Trader]++
				}
			})
		}
	})
	for mk, n := range perMaker {
		if n > 2*makerLevels {
			t.Fatalf("maker %d has %d resting orders, more than one ladder", mk, n)
		}
	}
}

func TestUnknownSymbol(t *testing.T) {
	x, _ := engine.OpenExchange([]string{"AAA"}, nil)
	defer x.Close()
	if err := Run(context.Background(), x, Config{Markets: []Market{{Symbol: "ZZZ", Ref: 1}}}); err == nil {
		t.Fatal("unknown symbol accepted")
	}
}
