package book

import (
	"reflect"
	"testing"
)

func limit(trader uint64, s Side, price, qty int64) Command {
	return Command{Kind: CmdNew, Type: Limit, TIF: GTC, Trader: trader, Side: s, Price: price, Qty: qty}
}

func market(trader uint64, s Side, qty int64) Command {
	return Command{Kind: CmdNew, Type: Market, TIF: IOC, Trader: trader, Side: s, Qty: qty}
}

func cancel(trader, id uint64) Command {
	return Command{Kind: CmdCancel, Trader: trader, OrderID: id}
}

// fill is the part of a Fill event the scenario tables assert on.
type fill struct {
	Maker, Taker uint64
	Price, Qty   int64
}

type scenario struct {
	name  string
	cmds  []Command
	fills []fill
	bids  []Level
	asks  []Level
}

func run(t *testing.T, sc scenario) (*Book, []Event) {
	t.Helper()
	b := New(0)
	var all []Event
	for i, c := range sc.cmds {
		c.Time = int64(i + 1)
		all = append(all, b.Apply(c)...)
		if err := checkInvariants(b); err != nil {
			t.Fatalf("after command %d: %v", i, err)
		}
	}
	var fills []fill
	for i, e := range all {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
		if e.Kind == EvFill {
			fills = append(fills, fill{e.MakerOrderID, e.OrderID, e.Price, e.Qty})
		}
	}
	if !reflect.DeepEqual(fills, sc.fills) {
		t.Errorf("fills\n got %v\nwant %v", fills, sc.fills)
	}
	if got := b.Depth(Buy, 0); !sameLevels(got, sc.bids) {
		t.Errorf("bids\n got %v\nwant %v", got, sc.bids)
	}
	if got := b.Depth(Sell, 0); !sameLevels(got, sc.asks) {
		t.Errorf("asks\n got %v\nwant %v", got, sc.asks)
	}
	return b, all
}

func sameLevels(a, b []Level) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestLimitAndMarketMatching(t *testing.T) {
	scenarios := []scenario{
		{
			name: "non-crossing limits rest on both sides",
			cmds: []Command{
				limit(1, Buy, 100, 10),
				limit(2, Sell, 102, 5),
				limit(1, Buy, 99, 7),
			},
			bids: []Level{{100, 10}, {99, 7}},
			asks: []Level{{102, 5}},
		},
		{
			name: "full fill removes the level",
			cmds: []Command{
				limit(1, Sell, 100, 10),
				limit(2, Buy, 100, 10),
			},
			fills: []fill{{1, 2, 100, 10}},
		},
		{
			name: "partial fill of the maker leaves its remainder at the front",
			cmds: []Command{
				limit(1, Sell, 100, 10),
				limit(2, Buy, 100, 4),
			},
			fills: []fill{{1, 2, 100, 4}},
			asks:  []Level{{100, 6}},
		},
		{
			name: "partial fill of the taker rests its remainder",
			cmds: []Command{
				limit(1, Sell, 100, 4),
				limit(2, Buy, 100, 10),
			},
			fills: []fill{{1, 2, 100, 4}},
			bids:  []Level{{100, 6}},
		},
		{
			name: "aggressive limit trades at the maker's price, not its own",
			cmds: []Command{
				limit(1, Sell, 100, 5),
				limit(2, Buy, 105, 5),
			},
			fills: []fill{{1, 2, 100, 5}},
		},
		{
			name: "taker sweeps several levels best price first",
			cmds: []Command{
				limit(1, Sell, 102, 5),
				limit(1, Sell, 100, 5),
				limit(1, Sell, 101, 5),
				limit(2, Buy, 102, 12),
			},
			fills: []fill{{2, 4, 100, 5}, {3, 4, 101, 5}, {1, 4, 102, 2}},
			asks:  []Level{{102, 3}},
		},
		{
			name: "sweep stops at the limit price and the rest is posted",
			cmds: []Command{
				limit(1, Sell, 100, 5),
				limit(1, Sell, 103, 5),
				limit(2, Buy, 101, 8),
			},
			fills: []fill{{1, 3, 100, 5}},
			bids:  []Level{{101, 3}},
			asks:  []Level{{103, 5}},
		},
		{
			name: "time priority within a level",
			cmds: []Command{
				limit(1, Buy, 100, 3),
				limit(2, Buy, 100, 3),
				limit(3, Buy, 100, 3),
				limit(4, Sell, 100, 7),
			},
			fills: []fill{{1, 4, 100, 3}, {2, 4, 100, 3}, {3, 4, 100, 1}},
			bids:  []Level{{100, 2}},
		},
		{
			name: "price beats time: a later better bid fills first",
			cmds: []Command{
				limit(1, Buy, 100, 5),
				limit(2, Buy, 101, 5),
				limit(3, Sell, 100, 6),
			},
			fills: []fill{{2, 3, 101, 5}, {1, 3, 100, 1}},
			bids:  []Level{{100, 4}},
		},
		{
			name: "market order walks the book and never rests",
			cmds: []Command{
				limit(1, Sell, 100, 5),
				limit(1, Sell, 110, 5),
				market(2, Buy, 8),
			},
			fills: []fill{{1, 3, 100, 5}, {2, 3, 110, 3}},
			asks:  []Level{{110, 2}},
		},
		{
			name: "market sell hits bids from the top",
			cmds: []Command{
				limit(1, Buy, 99, 5),
				limit(1, Buy, 100, 5),
				market(2, Sell, 6),
			},
			fills: []fill{{2, 3, 100, 5}, {1, 3, 99, 1}},
			bids:  []Level{{99, 4}},
		},
		{
			name: "cancelled order is skipped by later takers",
			cmds: []Command{
				limit(1, Sell, 100, 5),
				limit(2, Sell, 100, 5),
				cancel(1, 1),
				limit(3, Buy, 100, 5),
			},
			fills: []fill{{2, 3, 100, 5}},
		},
		{
			name: "cancel of the only order at a price removes the level",
			cmds: []Command{
				limit(1, Buy, 100, 5),
				limit(1, Buy, 99, 5),
				cancel(1, 1),
			},
			bids: []Level{{99, 5}},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}
}

func TestMarketOrderOnEmptyBook(t *testing.T) {
	b := New(0)
	evs := b.Apply(market(1, Buy, 10))
	if got, want := kinds(evs), []EventKind{EvAccepted, EvCancelled}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if evs[1].Qty != 10 || evs[1].Reason != ReasonNoLiquidity {
		t.Fatalf("cancel event %+v", evs[1])
	}
	if b.Len() != 0 {
		t.Fatalf("market order rested")
	}
}

func TestMarketOrderRemainderIsCancelled(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Sell, 100, 4))
	evs := b.Apply(market(2, Buy, 10))
	last := evs[len(evs)-1]
	if last.Kind != EvCancelled || last.Qty != 6 || last.Reason != ReasonNoLiquidity {
		t.Fatalf("last event %+v", last)
	}
	if _, _, ok := b.BestAsk(); ok {
		t.Fatalf("ask side should be empty")
	}
}

func TestCancel(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Buy, 100, 10))

	evs := b.Apply(cancel(2, 1))
	if evs[0].Kind != EvRejected || evs[0].Reason != ReasonNotOwner {
		t.Fatalf("cancel by another trader: %+v", evs[0])
	}
	evs = b.Apply(cancel(1, 99))
	if evs[0].Kind != EvRejected || evs[0].Reason != ReasonUnknownOrder {
		t.Fatalf("cancel of unknown id: %+v", evs[0])
	}

	evs = b.Apply(cancel(1, 1))
	if got, want := kinds(evs), []EventKind{EvCancelled, EvBookDelta}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if evs[0].Qty != 10 || evs[0].Reason != ReasonUserCancel {
		t.Fatalf("cancel event %+v", evs[0])
	}
	if evs[1].Side != Buy || evs[1].Price != 100 || evs[1].Qty != 0 {
		t.Fatalf("delta event %+v", evs[1])
	}

	evs = b.Apply(cancel(1, 1))
	if evs[0].Reason != ReasonUnknownOrder {
		t.Fatalf("second cancel: %+v", evs[0])
	}
}

func TestCancelAfterPartialFillReportsOpenQuantity(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Sell, 100, 10))
	b.Apply(limit(2, Buy, 100, 3))
	evs := b.Apply(cancel(1, 1))
	if evs[0].Qty != 7 {
		t.Fatalf("cancelled qty %d, want 7", evs[0].Qty)
	}
}

func TestEventsForACrossingOrder(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Sell, 100, 5))
	b.Apply(limit(1, Sell, 101, 5))
	evs := b.Apply(limit(2, Buy, 101, 12))

	want := []EventKind{EvAccepted, EvFill, EvBookDelta, EvFill, EvBookDelta, EvBookDelta}
	if got := kinds(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	f := evs[1]
	if f.OrderID != 3 || f.Trader != 2 || f.MakerOrderID != 1 || f.MakerTrader != 1 || f.Side != Buy || f.Remaining != 7 || f.MakerRemaining != 0 {
		t.Fatalf("fill %+v", f)
	}
	if d := evs[2]; d.Side != Sell || d.Price != 100 || d.Qty != 0 {
		t.Fatalf("ask delta %+v", d)
	}
	if d := evs[5]; d.Side != Buy || d.Price != 101 || d.Qty != 2 {
		t.Fatalf("bid delta %+v", d)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		cmd  Command
		want Reason
	}{
		{"zero trader", limit(0, Buy, 100, 1), ReasonInvalidTrader},
		{"bad side", limit(1, 0, 100, 1), ReasonInvalidSide},
		{"zero qty", limit(1, Buy, 100, 0), ReasonInvalidQty},
		{"negative qty", limit(1, Buy, 100, -5), ReasonInvalidQty},
		{"huge qty", limit(1, Buy, 100, MaxQty+1), ReasonInvalidQty},
		{"zero price", limit(1, Buy, 0, 1), ReasonInvalidPrice},
		{"negative price", limit(1, Sell, -1, 1), ReasonInvalidPrice},
		{"huge price", limit(1, Sell, MaxPrice+1, 1), ReasonInvalidPrice},
		{"market with price", Command{Kind: CmdNew, Type: Market, TIF: IOC, Trader: 1, Side: Buy, Price: 5, Qty: 1}, ReasonInvalidPrice},
		{"bad type", Command{Kind: CmdNew, Type: 9, TIF: GTC, Trader: 1, Side: Buy, Price: 5, Qty: 1}, ReasonInvalidType},
		{"bad kind", Command{Kind: 9, Trader: 1}, ReasonInvalidCommand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New(0)
			evs := b.Apply(tc.cmd)
			if len(evs) != 1 || evs[0].Kind != EvRejected || evs[0].Reason != tc.want {
				t.Fatalf("got %+v, want rejection %v", evs, tc.want)
			}
			if b.Len() != 0 {
				t.Fatalf("rejected order rested")
			}
		})
	}
}

func TestOrderIDsStartAfterBase(t *testing.T) {
	b := New(7 << 40)
	evs := b.Apply(limit(1, Buy, 100, 1))
	if evs[0].OrderID != 7<<40+1 {
		t.Fatalf("first id %d", evs[0].OrderID)
	}
}

func TestOrdersIteratesInPriorityOrder(t *testing.T) {
	b := New(0)
	for _, c := range []Command{limit(1, Buy, 100, 1), limit(2, Buy, 101, 1), limit(3, Buy, 100, 1), limit(4, Sell, 105, 1)} {
		b.Apply(c)
	}
	var ids []uint64
	b.Orders(Buy, func(o *Order) { ids = append(ids, o.ID) })
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 1 || ids[2] != 3 {
		t.Fatalf("bid order %v, want [2 1 3]", ids)
	}
}
