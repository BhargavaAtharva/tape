package book

import "math/rand/v2"

type Book struct {
	bids   side
	asks   side
	orders map[uint64]*Order
	// traders indexes resting orders by owner, for the open-order cap and
	// for listing one trader's orders without scanning the book.
	traders map[uint64]traderOrders

	seq    uint64
	nextID uint64
	now    int64
	events []Event

	expiry expiryHeap

	// Resting orders (and, in each side, price levels) are recycled through
	// free lists, so a book in steady state allocates nothing per command.
	freeOrders *Order
}

// New returns an empty book. Order ids are assigned as idBase+1, idBase+2, ...
// which lets a caller running several books give each a disjoint id range.
func New(idBase uint64) *Book {
	seed := rand.Uint64()
	return &Book{
		bids:    side{sign: -1, seed: seed},
		asks:    side{sign: 1, seed: seed},
		orders:  make(map[uint64]*Order),
		traders: make(map[uint64]traderOrders),
		nextID:  idBase + 1,
	}
}

// Seq is the sequence number of the last event emitted.
func (b *Book) Seq() uint64 { return b.seq }

// Len is the number of resting orders.
func (b *Book) Len() int { return len(b.orders) }

// Order returns the resting order with the given id, or nil. The pointer is
// only valid until the next Apply: orders that leave the book are recycled.
func (b *Book) Order(id uint64) *Order { return b.orders[id] }

func (b *Book) BestBid() (price, qty int64, ok bool) { return best(&b.bids) }
func (b *Book) BestAsk() (price, qty int64, ok bool) { return best(&b.asks) }

func best(s *side) (int64, int64, bool) {
	if s.best == nil {
		return 0, 0, false
	}
	return s.best.price, s.best.qty, true
}

// Depth returns up to max levels of one side, best first. max <= 0 means all.
func (b *Book) Depth(s Side, max int) []Level {
	sd := b.side(s)
	n := sd.n
	if max > 0 && max < n {
		n = max
	}
	out := make([]Level, 0, n)
	for lv := sd.best; lv != nil && len(out) < n; lv = lv.next {
		out = append(out, Level{Price: lv.price, Qty: lv.qty})
	}
	return out
}

// Orders calls fn for every resting order on one side, best price first and
// oldest first within a price. fn must not modify the book.
func (b *Book) Orders(s Side, fn func(*Order)) {
	for lv := b.side(s).best; lv != nil; lv = lv.next {
		for o := lv.head; o != nil; o = o.next {
			fn(o)
		}
	}
}

type traderOrders struct {
	head, tail *Order
	n          int
}

// OpenOrders is the number of orders the trader has resting.
func (b *Book) OpenOrders(trader uint64) int { return b.traders[trader].n }

// TraderOrders calls fn for each of the trader's resting orders, oldest
// first. fn must not modify the book.
func (b *Book) TraderOrders(trader uint64, fn func(*Order)) {
	for o := b.traders[trader].head; o != nil; o = o.tnext {
		fn(o)
	}
}

func (b *Book) index(o *Order) {
	t := b.traders[o.Trader]
	o.tprev, o.tnext = t.tail, nil
	if t.tail != nil {
		t.tail.tnext = o
	} else {
		t.head = o
	}
	t.tail = o
	t.n++
	b.orders[o.ID] = o
	b.traders[o.Trader] = t
}

func (b *Book) unindex(o *Order) {
	t := b.traders[o.Trader]
	if o.tprev != nil {
		o.tprev.tnext = o.tnext
	} else {
		t.head = o.tnext
	}
	if o.tnext != nil {
		o.tnext.tprev = o.tprev
	} else {
		t.tail = o.tprev
	}
	t.n--
	delete(b.orders, o.ID)
	if t.n == 0 {
		delete(b.traders, o.Trader)
	} else {
		b.traders[o.Trader] = t
	}
}

func (b *Book) side(s Side) *side {
	if s == Buy {
		return &b.bids
	}
	return &b.asks
}

func (b *Book) opposite(s Side) *side {
	if s == Buy {
		return &b.asks
	}
	return &b.bids
}

// Apply executes one command and returns the events it produced. The slice
// is reused by the next call; callers that keep events must copy them.
func (b *Book) Apply(c Command) []Event {
	b.events = b.events[:0]
	b.now = c.Time
	switch c.Kind {
	case CmdNew:
		b.newOrder(c)
	case CmdCancel:
		b.cancel(c)
	case CmdAmend:
		b.amend(c)
	case CmdExpire:
		b.expire()
	default:
		b.reject(c.OrderID, c.Trader, ReasonInvalidCommand)
	}
	return b.events
}

func (b *Book) emit(e Event) {
	b.seq++
	e.Seq = b.seq
	e.Time = b.now
	b.events = append(b.events, e)
}

func (b *Book) reject(id, trader uint64, r Reason) {
	b.emit(Event{Kind: EvRejected, OrderID: id, Trader: trader, Reason: r})
}

func (b *Book) delta(s Side, lv *level) {
	b.emit(Event{Kind: EvBookDelta, Side: s, Price: lv.price, Qty: lv.qty})
}

func validateNew(c Command) Reason {
	switch {
	case c.Trader == 0:
		return ReasonInvalidTrader
	case c.Side != Buy && c.Side != Sell:
		return ReasonInvalidSide
	case c.Type != Limit && c.Type != Market:
		return ReasonInvalidType
	case c.Qty <= 0 || c.Qty > MaxQty:
		return ReasonInvalidQty
	case c.Type == Limit && (c.Price <= 0 || c.Price > MaxPrice):
		return ReasonInvalidPrice
	case c.Type == Market && c.Price != 0:
		return ReasonInvalidPrice
	case c.TIF != GTC && c.TIF != IOC && c.TIF != FOK:
		return ReasonInvalidTIF
	case c.Type == Market && c.TIF == GTC:
		return ReasonInvalidTIF
	case c.PostOnly && (c.Type != Limit || c.TIF != GTC):
		return ReasonInvalidPostOnly
	case c.TTL < 0 || c.TTL > 0 && c.TIF != GTC:
		return ReasonInvalidTTL
	}
	return ReasonNone
}

// taker is an incoming order while it is being matched. It only becomes an
// Order if some quantity is left to rest.
type taker struct {
	id     uint64
	trader uint64
	side   Side
	market bool
	limit  int64
	qty    int64
	filled int64
	// expireAt carries an order's expiry through an amend that re-enters it.
	expireAt int64
}

func (t *taker) crosses(price int64) bool {
	if t.market {
		return true
	}
	if t.side == Buy {
		return price <= t.limit
	}
	return price >= t.limit
}

func (b *Book) newOrder(c Command) {
	if r := validateNew(c); r != ReasonNone {
		b.reject(0, c.Trader, r)
		return
	}
	if c.TIF == GTC && b.traders[c.Trader].n >= MaxOpenOrders {
		b.reject(0, c.Trader, ReasonTooManyOrders)
		return
	}
	t := taker{trader: c.Trader, side: c.Side, market: c.Type == Market, limit: c.Price, qty: c.Qty}
	if c.TTL > 0 {
		t.expireAt = c.Time + c.TTL
	}
	if c.PostOnly && b.wouldCross(&t) {
		b.reject(0, c.Trader, ReasonWouldCross)
		return
	}
	t.id = b.nextID
	b.nextID++
	b.emit(Event{Kind: EvAccepted, OrderID: t.id, Trader: c.Trader, Side: c.Side, Price: c.Price, Qty: c.Qty})

	if c.TIF == FOK && !b.fillable(&t) {
		b.cancelTaker(&t, ReasonFOK)
		return
	}
	selfTrade := b.match(&t)
	switch {
	case t.qty == 0:
	case selfTrade:
		b.cancelTaker(&t, ReasonSelfTrade)
	case t.market:
		b.cancelTaker(&t, ReasonNoLiquidity)
	case c.TIF == IOC:
		b.cancelTaker(&t, ReasonIOC)
	default:
		b.rest(&t, c.PostOnly)
		if t.expireAt > 0 {
			b.expiry.push(expiryEntry{at: t.expireAt, id: t.id})
		}
	}
}

func (b *Book) cancelTaker(t *taker, r Reason) {
	b.emit(Event{Kind: EvCancelled, OrderID: t.id, Trader: t.trader, Side: t.side, Price: t.limit, Qty: t.qty, Reason: r})
}

func (b *Book) wouldCross(t *taker) bool {
	lv := b.opposite(t.side).best
	return lv != nil && t.crosses(lv.price)
}

// fillable reports whether match would fill the taker completely. It has to
// look at individual orders rather than level totals because an order from
// the same trader stops matching (self-trade prevention), which would leave
// a fill-or-kill order partly filled.
func (b *Book) fillable(t *taker) bool {
	need := t.qty
	for lv := b.opposite(t.side).best; lv != nil && t.crosses(lv.price); lv = lv.next {
		for o := lv.head; o != nil; o = o.next {
			if o.Trader == t.trader {
				return false
			}
			need -= o.Qty
			if need <= 0 {
				return true
			}
		}
	}
	return false
}

// match trades the taker against the opposite side, best price first and
// oldest order first within a price. Every trade happens at the maker's
// price. One BookDelta is emitted per level touched, after its fills.
//
// Self-trade prevention is cancel-newest: when the next maker in line belongs
// to the taker's own trader, matching stops and match returns true so the
// caller cancels what is left of the taker. Fills made before that stand, and
// the resting order is untouched.
func (b *Book) match(t *taker) (selfTrade bool) {
	opp := b.opposite(t.side)
	makerSide := Sell
	if t.side == Sell {
		makerSide = Buy
	}
	for t.qty > 0 {
		lv := opp.best
		if lv == nil || !t.crosses(lv.price) {
			return false
		}
		before := lv.qty
		for t.qty > 0 && lv.head != nil {
			m := lv.head
			if m.Trader == t.trader {
				selfTrade = true
				break
			}
			q := min(t.qty, m.Qty)
			m.Qty -= q
			m.Filled += q
			lv.qty -= q
			t.qty -= q
			t.filled += q
			b.emit(Event{
				Kind: EvFill, Side: t.side, OrderID: t.id, Trader: t.trader,
				MakerOrderID: m.ID, MakerTrader: m.Trader,
				Price: lv.price, Qty: q, Remaining: t.qty, MakerRemaining: m.Qty,
			})
			if m.Qty == 0 {
				lv.unlink(m)
				b.unindex(m)
				b.freeOrder(m)
			}
		}
		if lv.qty != before {
			b.delta(makerSide, lv)
		}
		if lv.head == nil {
			opp.remove(lv)
		}
		if selfTrade {
			return true
		}
	}
	return false
}

func (b *Book) allocOrder() *Order {
	o := b.freeOrders
	if o == nil {
		return &Order{}
	}
	b.freeOrders = o.next
	o.next = nil
	return o
}

func (b *Book) freeOrder(o *Order) {
	*o = Order{next: b.freeOrders}
	b.freeOrders = o
}

// rest adds the remainder of a taker to the book as a resting order.
func (b *Book) rest(t *taker, postOnly bool) {
	o := b.allocOrder()
	o.ID, o.Trader, o.Price, o.Qty, o.Filled, o.Side, o.PostOnly = t.id, t.trader, t.limit, t.qty, t.filled, t.side, postOnly
	o.ExpireAt = t.expireAt
	lv := b.side(o.Side).getOrCreate(o.Price)
	lv.pushBack(o)
	b.index(o)
	b.delta(o.Side, lv)
}

func (b *Book) cancel(c Command) {
	o := b.orders[c.OrderID]
	if o == nil {
		b.reject(c.OrderID, c.Trader, ReasonUnknownOrder)
		return
	}
	if o.Trader != c.Trader {
		b.reject(c.OrderID, c.Trader, ReasonNotOwner)
		return
	}
	b.emit(Event{Kind: EvCancelled, OrderID: o.ID, Trader: o.Trader, Side: o.Side, Price: o.Price, Qty: o.Qty, Reason: ReasonUserCancel})
	b.remove(o)
}

func (b *Book) remove(o *Order) {
	lv := o.level
	s := b.side(o.Side)
	lv.unlink(o)
	b.unindex(o)
	b.delta(o.Side, lv)
	b.freeOrder(o)
	if lv.head == nil {
		s.remove(lv)
	}
}

// amend changes the price and/or total quantity of a resting order. Reducing
// the quantity at the same price keeps the order's place in the queue. Any
// other change takes the order out and re-enters it as a new arrival: it goes
// to the back of its level and, if the new price crosses, trades as a taker.
func (b *Book) amend(c Command) {
	o := b.orders[c.OrderID]
	if o == nil {
		b.reject(c.OrderID, c.Trader, ReasonUnknownOrder)
		return
	}
	if o.Trader != c.Trader {
		b.reject(c.OrderID, c.Trader, ReasonNotOwner)
		return
	}
	price, total := c.Price, c.Qty
	if price == 0 {
		price = o.Price
	}
	if total == 0 {
		total = o.Qty + o.Filled
	}
	switch {
	case price < 0 || price > MaxPrice:
		b.reject(c.OrderID, c.Trader, ReasonInvalidPrice)
		return
	case total < 0 || total > MaxQty:
		b.reject(c.OrderID, c.Trader, ReasonInvalidQty)
		return
	}
	open := total - o.Filled
	switch {
	case open <= 0:
		b.emit(Event{Kind: EvCancelled, OrderID: o.ID, Trader: o.Trader, Side: o.Side, Price: o.Price, Qty: o.Qty, Reason: ReasonAmendedToFilled})
		b.remove(o)
		return
	case price == o.Price && open == o.Qty:
		b.reject(c.OrderID, c.Trader, ReasonNoChange)
		return
	case price == o.Price && open < o.Qty:
		lv := o.level
		lv.qty -= o.Qty - open
		o.Qty = open
		b.emit(Event{Kind: EvAmended, OrderID: o.ID, Trader: o.Trader, Side: o.Side, Price: price, Qty: open})
		b.delta(o.Side, lv)
		return
	}

	t := taker{id: o.ID, trader: o.Trader, side: o.Side, limit: price, qty: open, filled: o.Filled, expireAt: o.ExpireAt}
	if o.PostOnly && b.wouldCross(&t) {
		b.reject(c.OrderID, c.Trader, ReasonWouldCross)
		return
	}
	postOnly := o.PostOnly
	b.emit(Event{Kind: EvAmended, OrderID: o.ID, Trader: o.Trader, Side: o.Side, Price: price, Qty: open})
	b.remove(o)
	selfTrade := b.match(&t)
	switch {
	case t.qty == 0:
	case selfTrade:
		b.cancelTaker(&t, ReasonSelfTrade)
	default:
		b.rest(&t, postOnly)
	}
}
