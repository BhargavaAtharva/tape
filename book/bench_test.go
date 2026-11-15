package book

import (
	"math/rand/v2"
	"testing"
)

// benchStream builds a realistic command stream: limit orders clustered
// around a slowly drifting mid, marketable orders, and cancels and amends
// aimed at orders that are actually resting at that moment. It is generated
// against a real book so the targets are right, then replayed on fresh books.
func benchStream(n int) []Command {
	rng := rand.New(rand.NewPCG(42, 7))
	b := New(0)
	var live []uint64
	mid := int64(100_000)
	cmds := make([]Command, 0, n)
	for i := range n {
		if i%64 == 0 {
			mid += rng.Int64N(21) - 10
		}
		trader := 1 + rng.Uint64N(200)
		var c Command
		switch r := rng.IntN(100); {
		case r < 25 && len(live) > 0:
			k := rng.IntN(len(live))
			c = Command{Kind: CmdCancel, OrderID: live[k]}
			live[k] = live[len(live)-1]
			live = live[:len(live)-1]
		case r < 30 && len(live) > 0:
			c = Command{Kind: CmdAmend, OrderID: live[rng.IntN(len(live))], Qty: 1 + rng.Int64N(5)}
		case r < 40:
			c = Command{Kind: CmdNew, Type: Market, TIF: IOC, Side: Side(1 + rng.IntN(2)), Qty: 1 + rng.Int64N(20)}
		default:
			side := Side(1 + rng.IntN(2))
			off := rng.Int64N(50) - 5
			price := mid - off
			if side == Sell {
				price = mid + off
			}
			c = Command{Kind: CmdNew, Type: Limit, TIF: GTC, Side: side, Price: price, Qty: 1 + rng.Int64N(20)}
		}
		if c.Kind == CmdNew {
			c.Trader = trader
		} else if o := b.Order(c.OrderID); o != nil {
			c.Trader = o.Trader
		}
		c.Time = int64(i)
		for _, e := range b.Apply(c) {
			if e.Kind == EvAccepted {
				live = append(live, e.OrderID)
			}
		}
		cmds = append(cmds, c)
		live = pruneLive(b, live)
	}
	return cmds
}

// pruneLive drops ids that are no longer resting, now and then, so cancel
// targets stay mostly valid without scanning on every command.
func pruneLive(b *Book, live []uint64) []uint64 {
	if len(live) < 2*b.Len()+64 {
		return live
	}
	kept := live[:0]
	for _, id := range live {
		if b.Order(id) != nil {
			kept = append(kept, id)
		}
	}
	return kept
}

func runStream(b *testing.B, cmds []Command) {
	b.ReportAllocs()
	bk := New(0)
	j := 0
	for b.Loop() {
		if j == len(cmds) {
			bk, j = New(0), 0
		}
		bk.Apply(cmds[j])
		j++
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "orders/s")
}

func BenchmarkApplyMixed(b *testing.B) {
	runStream(b, benchStream(200_000))
}

// BenchmarkRestAndCancel isolates book maintenance: every order rests on a
// fresh or existing level away from the touch and is then cancelled.
func BenchmarkRestAndCancel(b *testing.B) {
	cmds := make([]Command, 0, 200_000)
	// 1000 traders, so no one reaches the open-order cap.
	trader := func(id int) uint64 { return uint64(1 + (id-1)%1000) }
	for i := range 100_000 {
		cmds = append(cmds, Command{Kind: CmdNew, Type: Limit, TIF: GTC, Side: Buy, Trader: trader(i + 1), Price: int64(1000 + i%500), Qty: 5})
	}
	for i := range 100_000 {
		id := 100_000 - i
		cmds = append(cmds, Command{Kind: CmdCancel, Trader: trader(id), OrderID: uint64(id)})
	}
	runStream(b, cmds)
}

// BenchmarkSweep alternates five resting orders, one per level, with a
// market order that takes all five. ns/op is per command: one sweep costs
// six of them.
func BenchmarkSweep(b *testing.B) {
	var cmds []Command
	for range 20_000 {
		for p := range int64(5) {
			cmds = append(cmds, Command{Kind: CmdNew, Type: Limit, TIF: GTC, Side: Sell, Trader: 1, Price: 100 + p, Qty: 10})
		}
		cmds = append(cmds, Command{Kind: CmdNew, Type: Market, TIF: IOC, Side: Buy, Trader: 2, Qty: 50})
	}
	runStream(b, cmds)
}
