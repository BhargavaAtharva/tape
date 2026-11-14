package book

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
)

// account is what an outside observer can reconstruct about one order from
// the event stream alone.
type account struct {
	trader    uint64
	side      Side
	limit     int64
	market    bool
	submitted int64
	filled    int64
	cancelled int64
	open      int64
}

// ledger replays the event stream independently of the book and checks the
// book's claims against it after every command.
type ledger struct {
	lastSeq  uint64
	accounts map[uint64]*account
	live     int
	depth    [3]map[int64]int64
	queues   [3]map[int64][]uint64
}

func newLedger() *ledger {
	l := &ledger{accounts: make(map[uint64]*account)}
	l.depth[Buy] = make(map[int64]int64)
	l.depth[Sell] = make(map[int64]int64)
	l.queues[Buy] = make(map[int64][]uint64)
	l.queues[Sell] = make(map[int64][]uint64)
	return l
}

func (l *ledger) enqueue(id uint64, a *account) {
	l.queues[a.side][a.limit] = append(l.queues[a.side][a.limit], id)
}

func (l *ledger) dequeue(id uint64, a *account) {
	q := l.queues[a.side][a.limit]
	for i, x := range q {
		if x == id {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) == 0 {
		delete(l.queues[a.side], a.limit)
	} else {
		l.queues[a.side][a.limit] = q
	}
}

// front is the order that price-time priority says must trade next on a side.
func (l *ledger) front(s Side) (uint64, bool) {
	var bestPrice int64
	found := false
	for p := range l.queues[s] {
		if !found || s == Buy && p > bestPrice || s == Sell && p < bestPrice {
			bestPrice, found = p, true
		}
	}
	if !found {
		return 0, false
	}
	return l.queues[s][bestPrice][0], true
}

func (l *ledger) setOpen(a *account, open int64) {
	if a.open > 0 && open == 0 {
		l.live--
	}
	if a.open == 0 && open > 0 {
		l.live++
	}
	a.open = open
}

func (l *ledger) observe(c Command, evs []Event) error {
	if len(evs) == 0 {
		if c.Kind == CmdExpire {
			return nil
		}
		return fmt.Errorf("command produced no events")
	}
	// arriving is the order that entered (or re-entered) the market with this
	// command; it joins the back of its queue if anything is left at the end.
	var arriving uint64
	defer func() {
		if a := l.accounts[arriving]; a != nil && a.open > 0 {
			l.enqueue(arriving, a)
		}
	}()
	for i, e := range evs {
		if e.Seq != l.lastSeq+1 {
			return fmt.Errorf("seq %d follows %d", e.Seq, l.lastSeq)
		}
		l.lastSeq = e.Seq
		if e.Time != c.Time {
			return fmt.Errorf("seq %d: event time %d, command time %d", e.Seq, e.Time, c.Time)
		}
		switch e.Kind {
		case EvRejected:
			if len(evs) != 1 {
				return fmt.Errorf("seq %d: rejection mixed with %d other events", e.Seq, len(evs)-1)
			}
		case EvAccepted:
			if i != 0 {
				return fmt.Errorf("seq %d: accepted is not the first event", e.Seq)
			}
			if l.accounts[e.OrderID] != nil {
				return fmt.Errorf("seq %d: order id %d reused", e.Seq, e.OrderID)
			}
			a := &account{trader: e.Trader, side: e.Side, limit: e.Price, market: c.Type == Market, submitted: e.Qty}
			l.accounts[e.OrderID] = a
			l.setOpen(a, e.Qty)
			arriving = e.OrderID
		case EvFill:
			if err := l.fill(e); err != nil {
				return fmt.Errorf("seq %d: %w", e.Seq, err)
			}
		case EvCancelled:
			a := l.accounts[e.OrderID]
			if a == nil || a.open == 0 {
				return fmt.Errorf("seq %d: cancel of order %d which is not open", e.Seq, e.OrderID)
			}
			if e.Qty != a.open {
				return fmt.Errorf("seq %d: cancelled %d, open was %d", e.Seq, e.Qty, a.open)
			}
			a.cancelled += e.Qty
			if e.OrderID != arriving {
				l.dequeue(e.OrderID, a)
			}
			l.setOpen(a, 0)
		case EvAmended:
			a := l.accounts[e.OrderID]
			if a == nil || a.open == 0 {
				return fmt.Errorf("seq %d: amend of order %d which is not open", e.Seq, e.OrderID)
			}
			if e.Price != a.limit || e.Qty > a.open {
				l.dequeue(e.OrderID, a)
				arriving = e.OrderID
			}
			if e.Qty > a.open {
				a.submitted += e.Qty - a.open
			} else {
				a.cancelled += a.open - e.Qty
			}
			a.limit = e.Price
			l.setOpen(a, e.Qty)
		case EvBookDelta:
			if e.Qty < 0 {
				return fmt.Errorf("seq %d: negative level quantity", e.Seq)
			}
			if e.Qty == 0 {
				delete(l.depth[e.Side], e.Price)
			} else {
				l.depth[e.Side][e.Price] = e.Qty
			}
		default:
			return fmt.Errorf("seq %d: unknown event kind %d", e.Seq, e.Kind)
		}
	}
	return nil
}

func (l *ledger) fill(e Event) error {
	taker, maker := l.accounts[e.OrderID], l.accounts[e.MakerOrderID]
	if taker == nil || maker == nil {
		return fmt.Errorf("fill references unknown order")
	}
	if e.Qty <= 0 || e.Qty > taker.open || e.Qty > maker.open {
		return fmt.Errorf("fill of %d exceeds open quantity (taker %d, maker %d)", e.Qty, taker.open, maker.open)
	}
	if taker.side != e.Side || maker.side == taker.side {
		return fmt.Errorf("fill between orders on the same side")
	}
	if taker.trader == maker.trader {
		return fmt.Errorf("self trade for trader %d", taker.trader)
	}
	if id, ok := l.front(maker.side); !ok || id != e.MakerOrderID {
		return fmt.Errorf("maker %d filled out of turn, order %d has priority", e.MakerOrderID, id)
	}
	if e.Price != maker.limit {
		return fmt.Errorf("fill at %d, maker's limit is %d", e.Price, maker.limit)
	}
	if !taker.market {
		if taker.side == Buy && e.Price > taker.limit || taker.side == Sell && e.Price < taker.limit {
			return fmt.Errorf("fill at %d violates the taker's limit %d", e.Price, taker.limit)
		}
	}
	taker.filled += e.Qty
	maker.filled += e.Qty
	l.setOpen(taker, taker.open-e.Qty)
	l.setOpen(maker, maker.open-e.Qty)
	if maker.open == 0 {
		l.dequeue(e.MakerOrderID, maker)
	}
	if e.Remaining != taker.open || e.MakerRemaining != maker.open {
		return fmt.Errorf("fill reports remaining %d/%d, ledger has %d/%d", e.Remaining, e.MakerRemaining, taker.open, maker.open)
	}
	return nil
}

// reconcile compares the ledger with the book itself and checks quantity
// conservation for every order the command's events touched.
func (l *ledger) reconcile(b *Book, evs []Event) error {
	if err := checkInvariants(b); err != nil {
		return err
	}
	if b.Seq() != l.lastSeq {
		return fmt.Errorf("book seq %d, last event %d", b.Seq(), l.lastSeq)
	}
	if l.live != len(b.orders) {
		return fmt.Errorf("events imply %d open orders, book holds %d", l.live, len(b.orders))
	}
	for id, o := range b.orders {
		a := l.accounts[id]
		if a == nil {
			return fmt.Errorf("order %d rests without ever being accepted", id)
		}
		if a.open != o.Qty || a.limit != o.Price || a.side != o.Side || a.trader != o.Trader {
			return fmt.Errorf("order %d: book has %d@%d, events imply %d@%d", id, o.Qty, o.Price, a.open, a.limit)
		}
	}
	for _, e := range evs {
		for _, id := range [2]uint64{e.OrderID, e.MakerOrderID} {
			a := l.accounts[id]
			if a != nil && a.submitted != a.filled+a.cancelled+a.open {
				return fmt.Errorf("order %d: submitted %d != filled %d + cancelled %d + open %d", id, a.submitted, a.filled, a.cancelled, a.open)
			}
		}
	}
	for _, s := range []Side{Buy, Sell} {
		levels := b.Depth(s, 0)
		if len(levels) != len(l.depth[s]) {
			return fmt.Errorf("%v: book has %d levels, deltas imply %d", s, len(levels), len(l.depth[s]))
		}
		for _, lv := range levels {
			if l.depth[s][lv.Price] != lv.Qty {
				return fmt.Errorf("%v level %d: book has %d, deltas imply %d", s, lv.Price, lv.Qty, l.depth[s][lv.Price])
			}
		}
	}
	return nil
}

const genWidth = 6

// decode turns six bytes into a command. Prices stay in a narrow band and
// only four traders exist so that crossing, queueing at a shared price and
// self-trades all happen constantly.
func decode(p []byte, issued uint64) Command {
	trader := uint64(1 + p[1]%4)
	target := uint64(p[4]) % (issued + 2)
	switch op := p[0] % 20; {
	case op < 12:
		c := Command{Kind: CmdNew, Trader: trader, Side: Buy, Type: Limit, TIF: GTC, Price: 90 + int64(p[3]%21), Qty: 1 + int64(p[5]%20)}
		if p[2]&1 == 1 {
			c.Side = Sell
		}
		switch (p[2] >> 1) % 10 {
		case 5:
			c.TIF = IOC
		case 6:
			c.TIF = FOK
		case 7:
			c.Type, c.TIF, c.Price = Market, IOC, 0
		case 8:
			c.PostOnly = true
		case 9:
			c.Type, c.TIF, c.Price = Market, FOK, 0
		}
		if c.TIF == GTC && p[4]%3 == 0 {
			c.TTL = 1 + int64(p[5]%40)
		}
		return c
	case op < 16:
		return Command{Kind: CmdCancel, Trader: trader, OrderID: target}
	case op < 19:
		c := Command{Kind: CmdAmend, Trader: trader, OrderID: target, Qty: int64(p[5] % 25)}
		if p[3]%3 != 0 {
			c.Price = 90 + int64(p[3]%21)
		}
		return c
	case p[1]%2 == 0:
		return Command{Kind: CmdExpire}
	default:
		// Deliberately malformed: every field straight from the input.
		return Command{
			Kind: CommandKind(p[1] % 5), Side: Side(p[2] % 4), Type: OrderType(p[3] % 4), TIF: TimeInForce(p[4] % 5),
			PostOnly: p[5]&1 == 1, Trader: uint64(p[1] % 3), OrderID: target,
			Price: int64(int8(p[3])), Qty: int64(int8(p[5])),
		}
	}
}

// drive applies the encoded command stream to a fresh book, checking every
// invariant after each command. Halfway through it clones the book through a
// snapshot and requires the clone to stay event-for-event identical.
func drive(data []byte) error {
	b := New(0)
	l := newLedger()
	var twin *Book
	n := len(data) / genWidth
	for i := range n {
		if i == n/2 {
			var err error
			if twin, err = Restore(b.AppendSnapshot(nil)); err != nil {
				return fmt.Errorf("command %d: restore: %w", i, err)
			}
		}
		c := decode(data[i*genWidth:], b.nextID-1)
		c.Time = int64(i)
		evs := b.Apply(c)
		if err := l.observe(c, evs); err != nil {
			return fmt.Errorf("command %d %+v: %w", i, c, err)
		}
		if err := l.reconcile(b, evs); err != nil {
			return fmt.Errorf("command %d %+v: %w", i, c, err)
		}
		if twin != nil {
			if tw := twin.Apply(c); !reflect.DeepEqual(tw, evs) {
				return fmt.Errorf("command %d %+v: restored twin diverged", i, c)
			}
		}
	}
	if twin != nil && twin.Hash() != b.Hash() {
		return fmt.Errorf("restored twin ends with a different hash")
	}
	return nil
}

func TestRandomCommandStreams(t *testing.T) {
	seeds, length := 200, 2000
	if testing.Short() {
		seeds = 20
	}
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 0x7a9e))
		data := make([]byte, length*genWidth)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		if err := drive(data); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
}

func FuzzBook(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 10, 0, 4, 0, 1, 1, 10, 0, 9})
	f.Add([]byte{0, 0, 1, 12, 0, 9, 0, 0, 0, 12, 0, 3, 16, 0, 0, 5, 1, 2, 12, 0, 0, 0, 1, 0})
	rng := rand.New(rand.NewPCG(1, 2))
	seed := make([]byte, 600)
	for i := range seed {
		seed[i] = byte(rng.Uint32())
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4000*genWidth {
			data = data[:4000*genWidth]
		}
		if err := drive(data); err != nil {
			t.Fatal(err)
		}
	})
}
