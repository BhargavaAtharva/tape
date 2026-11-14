package book

import (
	"reflect"
	"testing"
)

func ioc(trader uint64, s Side, price, qty int64) Command {
	c := limit(trader, s, price, qty)
	c.TIF = IOC
	return c
}

func fok(trader uint64, s Side, price, qty int64) Command {
	c := limit(trader, s, price, qty)
	c.TIF = FOK
	return c
}

func postOnly(trader uint64, s Side, price, qty int64) Command {
	c := limit(trader, s, price, qty)
	c.PostOnly = true
	return c
}

func amend(trader, id uint64, price, qty int64) Command {
	return Command{Kind: CmdAmend, Trader: trader, OrderID: id, Price: price, Qty: qty}
}

func lastEvent(evs []Event, k EventKind) (Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == k {
			return evs[i], true
		}
	}
	return Event{}, false
}

func TestIOC(t *testing.T) {
	scenarios := []scenario{
		{
			name: "fills what it can and never rests",
			cmds: []Command{
				limit(1, Sell, 100, 4),
				limit(1, Sell, 105, 4),
				ioc(2, Buy, 100, 10),
			},
			fills: []fill{{1, 3, 100, 4}},
			asks:  []Level{{105, 4}},
		},
		{
			name: "no cross means nothing happens to the book",
			cmds: []Command{
				limit(1, Sell, 100, 4),
				ioc(2, Buy, 99, 10),
			},
			asks: []Level{{100, 4}},
		},
		{
			name: "fully filled ioc",
			cmds: []Command{
				limit(1, Buy, 100, 10),
				ioc(2, Sell, 100, 10),
			},
			fills: []fill{{1, 2, 100, 10}},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}

	b := New(0)
	b.Apply(limit(1, Sell, 100, 4))
	evs := b.Apply(ioc(2, Buy, 100, 10))
	c, ok := lastEvent(evs, EvCancelled)
	if !ok || c.Qty != 6 || c.Reason != ReasonIOC {
		t.Fatalf("ioc remainder: %+v", evs)
	}
}

func TestFOK(t *testing.T) {
	scenarios := []scenario{
		{
			name: "fills completely across levels",
			cmds: []Command{
				limit(1, Sell, 100, 4),
				limit(1, Sell, 101, 6),
				fok(2, Buy, 101, 10),
			},
			fills: []fill{{1, 3, 100, 4}, {2, 3, 101, 6}},
		},
		{
			name: "killed without any fill when liquidity inside the limit is short",
			cmds: []Command{
				limit(1, Sell, 100, 4),
				limit(1, Sell, 101, 5),
				limit(1, Sell, 102, 50),
				fok(2, Buy, 101, 10),
			},
			asks: []Level{{100, 4}, {101, 5}, {102, 50}},
		},
		{
			name: "killed on an empty book",
			cmds: []Command{fok(2, Sell, 100, 1)},
		},
		{
			name: "own resting order in the path makes it unfillable",
			cmds: []Command{
				limit(1, Sell, 100, 4),
				limit(2, Sell, 100, 4),
				limit(1, Sell, 101, 10),
				fok(2, Buy, 101, 10),
			},
			asks: []Level{{100, 8}, {101, 10}},
		},
		{
			name: "own resting order behind the needed quantity does not matter",
			cmds: []Command{
				limit(1, Sell, 100, 10),
				limit(2, Sell, 100, 4),
				fok(2, Buy, 100, 10),
			},
			fills: []fill{{1, 3, 100, 10}},
			asks:  []Level{{100, 4}},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}

	b := New(0)
	b.Apply(limit(1, Sell, 100, 4))
	evs := b.Apply(fok(2, Buy, 100, 5))
	if got, want := kinds(evs), []EventKind{EvAccepted, EvCancelled}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if evs[1].Qty != 5 || evs[1].Reason != ReasonFOK {
		t.Fatalf("kill event %+v", evs[1])
	}

	mfok := market(2, Buy, 4)
	mfok.TIF = FOK
	evs = b.Apply(mfok)
	if f, ok := lastEvent(evs, EvFill); !ok || f.Qty != 4 {
		t.Fatalf("market fok should fill: %+v", evs)
	}
}

func TestPostOnly(t *testing.T) {
	scenarios := []scenario{
		{
			name: "rests when it does not cross",
			cmds: []Command{
				limit(1, Sell, 101, 5),
				postOnly(2, Buy, 100, 5),
			},
			bids: []Level{{100, 5}},
			asks: []Level{{101, 5}},
		},
		{
			name: "rejected when it would take liquidity",
			cmds: []Command{
				limit(1, Sell, 101, 5),
				postOnly(2, Buy, 101, 5),
			},
			asks: []Level{{101, 5}},
		},
		{
			name: "rejected sell through the bid",
			cmds: []Command{
				limit(1, Buy, 100, 5),
				postOnly(2, Sell, 99, 5),
			},
			bids: []Level{{100, 5}},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}

	b := New(0)
	b.Apply(limit(1, Sell, 101, 5))
	evs := b.Apply(postOnly(2, Buy, 101, 5))
	if len(evs) != 1 || evs[0].Kind != EvRejected || evs[0].Reason != ReasonWouldCross {
		t.Fatalf("crossing post-only: %+v", evs)
	}

	for _, c := range []Command{
		{Kind: CmdNew, Type: Limit, TIF: IOC, PostOnly: true, Trader: 1, Side: Buy, Price: 1, Qty: 1},
		{Kind: CmdNew, Type: Market, TIF: IOC, PostOnly: true, Trader: 1, Side: Buy, Qty: 1},
	} {
		evs := b.Apply(c)
		if evs[0].Kind != EvRejected || evs[0].Reason != ReasonInvalidPostOnly {
			t.Fatalf("post-only with tif %v type %v: %+v", c.TIF, c.Type, evs)
		}
	}
}

func TestTimeInForceValidation(t *testing.T) {
	b := New(0)
	for _, c := range []Command{
		{Kind: CmdNew, Type: Limit, Trader: 1, Side: Buy, Price: 1, Qty: 1},
		{Kind: CmdNew, Type: Limit, TIF: 7, Trader: 1, Side: Buy, Price: 1, Qty: 1},
		{Kind: CmdNew, Type: Market, TIF: GTC, Trader: 1, Side: Buy, Qty: 1},
	} {
		evs := b.Apply(c)
		if evs[0].Kind != EvRejected || evs[0].Reason != ReasonInvalidTIF {
			t.Fatalf("tif %v type %v: %+v", c.TIF, c.Type, evs)
		}
	}
}

func TestAmendQuantityDownKeepsPriority(t *testing.T) {
	_, evs := run(t, scenario{
		cmds: []Command{
			limit(1, Buy, 100, 10),
			limit(2, Buy, 100, 10),
			amend(1, 1, 0, 4),
			limit(3, Sell, 100, 6),
		},
		fills: []fill{{1, 3, 100, 4}, {2, 3, 100, 2}},
		bids:  []Level{{100, 8}},
	})
	a, ok := lastEvent(evs, EvAmended)
	if !ok || a.OrderID != 1 || a.Price != 100 || a.Qty != 4 {
		t.Fatalf("amended event %+v", a)
	}
}

func TestAmendQuantityUpLosesPriority(t *testing.T) {
	run(t, scenario{
		cmds: []Command{
			limit(1, Buy, 100, 5),
			limit(2, Buy, 100, 5),
			amend(1, 1, 0, 8),
			limit(3, Sell, 100, 6),
		},
		fills: []fill{{2, 3, 100, 5}, {1, 3, 100, 1}},
		bids:  []Level{{100, 7}},
	})
}

func TestAmendPriceLosesPriority(t *testing.T) {
	run(t, scenario{
		cmds: []Command{
			limit(1, Buy, 99, 5),
			limit(2, Buy, 100, 5),
			amend(1, 1, 100, 0),
			limit(3, Sell, 100, 6),
		},
		fills: []fill{{2, 3, 100, 5}, {1, 3, 100, 1}},
		bids:  []Level{{100, 4}},
	})
}

func TestAmendToSamePriceWithExplicitPriceStillKeepsPriority(t *testing.T) {
	run(t, scenario{
		cmds: []Command{
			limit(1, Buy, 100, 10),
			limit(2, Buy, 100, 10),
			amend(1, 1, 100, 4),
			limit(3, Sell, 100, 4),
		},
		fills: []fill{{1, 3, 100, 4}},
		bids:  []Level{{100, 10}},
	})
}

func TestAmendIntoTheSpreadTradesAsTaker(t *testing.T) {
	_, evs := run(t, scenario{
		cmds: []Command{
			limit(1, Sell, 101, 3),
			limit(2, Buy, 99, 5),
			amend(2, 2, 101, 0),
		},
		fills: []fill{{1, 2, 101, 3}},
		bids:  []Level{{101, 2}},
	})
	f, _ := lastEvent(evs, EvFill)
	if f.Side != Buy || f.Remaining != 2 {
		t.Fatalf("fill %+v", f)
	}
}

func TestAmendRejections(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Sell, 101, 5))
	b.Apply(postOnly(2, Buy, 99, 5))

	cases := []struct {
		name string
		cmd  Command
		want Reason
	}{
		{"unknown order", amend(1, 42, 100, 1), ReasonUnknownOrder},
		{"not owner", amend(2, 1, 100, 1), ReasonNotOwner},
		{"nothing to change", amend(1, 1, 0, 0), ReasonNoChange},
		{"same values", amend(1, 1, 101, 5), ReasonNoChange},
		{"negative price", amend(1, 1, -1, 0), ReasonInvalidPrice},
		{"negative qty", amend(1, 1, 0, -1), ReasonInvalidQty},
		{"huge qty", amend(1, 1, 0, MaxQty+1), ReasonInvalidQty},
		{"post-only order amended across the spread", amend(2, 2, 101, 0), ReasonWouldCross},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := b.Apply(tc.cmd)
			if len(evs) != 1 || evs[0].Kind != EvRejected || evs[0].Reason != tc.want {
				t.Fatalf("got %+v, want rejection %v", evs, tc.want)
			}
			if err := checkInvariants(b); err != nil {
				t.Fatal(err)
			}
		})
	}
	if got := b.Depth(Buy, 0); !sameLevels(got, []Level{{99, 5}}) {
		t.Fatalf("rejected amends changed the bids: %v", got)
	}
	if got := b.Depth(Sell, 0); !sameLevels(got, []Level{{101, 5}}) {
		t.Fatalf("rejected amends changed the asks: %v", got)
	}
}

func TestSelfTradePrevention(t *testing.T) {
	scenarios := []scenario{
		{
			name: "incoming order is cancelled, resting order stays",
			cmds: []Command{
				limit(1, Sell, 100, 5),
				limit(1, Buy, 100, 5),
			},
			asks: []Level{{100, 5}},
		},
		{
			name: "fills ahead of the own order stand, the rest is cancelled",
			cmds: []Command{
				limit(2, Sell, 100, 3),
				limit(1, Sell, 100, 5),
				limit(3, Sell, 100, 5),
				limit(1, Buy, 100, 10),
			},
			fills: []fill{{1, 4, 100, 3}},
			asks:  []Level{{100, 10}},
		},
		{
			name: "own order at a worse level is only hit if reached",
			cmds: []Command{
				limit(2, Sell, 100, 5),
				limit(1, Sell, 101, 5),
				limit(1, Buy, 100, 5),
			},
			fills: []fill{{1, 3, 100, 5}},
			asks:  []Level{{101, 5}},
		},
		{
			name: "market order against own quote",
			cmds: []Command{
				limit(1, Buy, 100, 5),
				market(1, Sell, 5),
			},
			bids: []Level{{100, 5}},
		},
		{
			name: "amend into own order cancels the amended order",
			cmds: []Command{
				limit(1, Sell, 101, 5),
				limit(1, Buy, 99, 5),
				amend(1, 2, 101, 0),
			},
			asks: []Level{{101, 5}},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}

	b := New(0)
	b.Apply(limit(2, Sell, 100, 3))
	b.Apply(limit(1, Sell, 100, 5))
	evs := b.Apply(limit(1, Buy, 100, 10))
	want := []EventKind{EvAccepted, EvFill, EvBookDelta, EvCancelled}
	if got := kinds(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if c := evs[3]; c.Qty != 7 || c.Reason != ReasonSelfTrade || c.OrderID != 3 {
		t.Fatalf("cancel event %+v", c)
	}
}

func TestOpenOrderCap(t *testing.T) {
	b := New(0)
	for i := range MaxOpenOrders {
		if evs := b.Apply(limit(1, Buy, int64(100+i%5), 1)); evs[0].Kind != EvAccepted {
			t.Fatalf("order %d: %+v", i, evs[0])
		}
	}
	evs := b.Apply(limit(1, Buy, 100, 1))
	if evs[0].Kind != EvRejected || evs[0].Reason != ReasonTooManyOrders {
		t.Fatalf("order over the cap: %+v", evs)
	}
	// Orders that cannot rest are not limited, and other traders are not affected.
	if evs := b.Apply(ioc(1, Buy, 100, 1)); evs[0].Kind != EvAccepted {
		t.Fatalf("IOC over the cap: %+v", evs[0])
	}
	if evs := b.Apply(limit(2, Buy, 100, 1)); evs[0].Kind != EvAccepted {
		t.Fatalf("other trader: %+v", evs[0])
	}
	b.Apply(cancel(1, 1))
	if b.OpenOrders(1) != MaxOpenOrders-1 {
		t.Fatalf("open orders %d after a cancel", b.OpenOrders(1))
	}
	if evs := b.Apply(limit(1, Buy, 100, 1)); evs[0].Kind != EvAccepted {
		t.Fatalf("order after a cancel freed a slot: %+v", evs[0])
	}
	var ids []uint64
	b.TraderOrders(2, func(o *Order) { ids = append(ids, o.ID) })
	if len(ids) != 1 {
		t.Fatalf("trader 2 orders %v", ids)
	}
	if err := checkInvariants(b); err != nil {
		t.Fatal(err)
	}
}

// Amend quantities are totals. An order of 10 that has filled 6 and is
// amended to 8 has 2 left open, and keeps its place because that is a
// reduction; amended to 5, less than has filled, it is finished.
func TestAmendQuantityIsTotalIncludingFills(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Buy, 100, 10))
	b.Apply(limit(2, Buy, 100, 10))
	b.Apply(limit(3, Sell, 100, 6))
	evs := b.Apply(amend(1, 1, 0, 8))
	if evs[0].Kind != EvAmended || evs[0].Qty != 2 {
		t.Fatalf("amend to total 8: %+v", evs)
	}
	if o := b.Order(1); o.Qty != 2 || o.Filled != 6 {
		t.Fatalf("order after amend: %+v", o)
	}
	evs = b.Apply(limit(3, Sell, 100, 1))
	if f, _ := lastEvent(evs, EvFill); f.MakerOrderID != 1 {
		t.Fatalf("amended order lost its priority: %+v", f)
	}

	b = New(0)
	b.Apply(limit(1, Buy, 99, 10))
	b.Apply(limit(3, Sell, 99, 6))
	evs = b.Apply(amend(1, 1, 0, 5))
	if evs[0].Kind != EvCancelled || evs[0].Reason != ReasonAmendedToFilled || evs[0].Qty != 4 || b.Order(1) != nil {
		t.Fatalf("amend below filled: %+v", evs)
	}
	if err := checkInvariants(b); err != nil {
		t.Fatal(err)
	}
}

func TestFilledSurvivesSnapshot(t *testing.T) {
	b := New(0)
	b.Apply(limit(1, Buy, 100, 10))
	b.Apply(limit(2, Sell, 100, 4))
	r, err := Restore(b.AppendSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if o := r.Order(1); o == nil || o.Filled != 4 || o.Qty != 6 {
		t.Fatalf("restored order %+v", o)
	}
}

func TestOrdersExpireAfterTheirTTL(t *testing.T) {
	b := New(0)
	at := func(c Command, t int64) Command { c.Time = t; return c }
	withTTL := func(c Command, ttl int64) Command { c.TTL = ttl; return c }

	b.Apply(at(withTTL(limit(1, Buy, 100, 5), 10), 0))  // id 1, expires at 10
	b.Apply(at(withTTL(limit(2, Buy, 99, 5), 5), 1))    // id 2, expires at 6
	b.Apply(at(withTTL(limit(3, Sell, 105, 5), 20), 2)) // id 3, expires at 22
	b.Apply(at(limit(4, Sell, 106, 5), 2))              // id 4, never expires
	b.Apply(at(amend(1, 1, 101, 0), 3))                 // re-entered, keeps its expiry
	b.Apply(at(limit(5, Sell, 101, 2), 4))              // partly fills id 1

	if at, ok := b.NextExpiry(); !ok || at != 6 {
		t.Fatalf("next expiry %d %v, want 6", at, ok)
	}
	if evs := b.Apply(Command{Kind: CmdExpire, Time: 5}); len(evs) != 0 {
		t.Fatalf("expired early: %+v", evs)
	}
	evs := b.Apply(Command{Kind: CmdExpire, Time: 10})
	var expired []uint64
	for _, e := range evs {
		if e.Kind == EvCancelled {
			if e.Reason != ReasonExpired {
				t.Fatalf("cancel reason %v", e.Reason)
			}
			expired = append(expired, e.OrderID)
		}
	}
	if len(expired) != 2 || expired[0] != 2 || expired[1] != 1 {
		t.Fatalf("expired %v, want [2 1] in expiry order", expired)
	}
	r, err := Restore(b.AppendSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := r.NextExpiry(); !ok || at != 22 {
		t.Fatalf("restored next expiry %d %v", at, ok)
	}
	r.Apply(Command{Kind: CmdExpire, Time: 100})
	if r.Len() != 1 || r.Order(4) == nil {
		t.Fatalf("after final expiry %d orders left", r.Len())
	}
	if _, ok := r.NextExpiry(); ok {
		t.Fatal("expiry left after every expiring order is gone")
	}
	if evs := b.Apply(withTTL(ioc(1, Buy, 100, 1), 5)); evs[0].Reason != ReasonInvalidTTL {
		t.Fatalf("TTL on IOC: %+v", evs[0])
	}
}

func TestValidateIsStatic(t *testing.T) {
	for _, tc := range []struct {
		c    Command
		want Reason
	}{
		{limit(1, Buy, 100, 1), ReasonNone},
		{limit(0, Buy, 100, 1), ReasonInvalidTrader},
		{limit(1, Buy, 0, 1), ReasonInvalidPrice},
		{cancel(1, 42), ReasonNone},
		{cancel(1, 0), ReasonUnknownOrder},
		{amend(1, 5, 0, 0), ReasonNoChange},
		{amend(1, 5, -1, 0), ReasonInvalidPrice},
		{amend(0, 5, 0, 3), ReasonInvalidTrader},
		{Command{Kind: CmdExpire}, ReasonInvalidCommand},
		{Command{Kind: 9}, ReasonInvalidCommand},
	} {
		if got := Validate(tc.c); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestOrderIDsRunOut(t *testing.T) {
	b := New(3 << OrderIDBits)
	b.nextID = 3<<OrderIDBits + maxOrderID
	if evs := b.Apply(limit(1, Buy, 100, 1)); evs[0].Kind != EvAccepted || evs[0].OrderID != 4<<OrderIDBits-1 {
		t.Fatalf("last id: %+v", evs[0])
	}
	if evs := b.Apply(limit(1, Buy, 100, 1)); evs[0].Reason != ReasonIDsExhausted {
		t.Fatalf("past the last id: %+v", evs[0])
	}
	r, err := Restore(b.AppendSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if evs := r.Apply(limit(1, Buy, 100, 1)); evs[0].Reason != ReasonIDsExhausted {
		t.Fatalf("restored book forgot its id range: %+v", evs[0])
	}
}
