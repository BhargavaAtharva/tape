package book

type Book struct {
	bids   side
	asks   side
	orders map[uint64]*Order

	seq    uint64
	nextID uint64
	now    int64
	events []Event
}

// New returns an empty book. Order ids are assigned as idBase+1, idBase+2, ...
// which lets a caller running several books give each a disjoint id range.
func New(idBase uint64) *Book {
	return &Book{
		bids:   side{sign: -1},
		asks:   side{sign: 1},
		orders: make(map[uint64]*Order),
		nextID: idBase + 1,
	}
}

// Seq is the sequence number of the last event emitted.
func (b *Book) Seq() uint64 { return b.seq }

// Len is the number of resting orders.
func (b *Book) Len() int { return len(b.orders) }

// Order returns the resting order with the given id, or nil.
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

// Apply executes one command and returns the events it produced.
func (b *Book) Apply(c Command) []Event {
	b.events = nil
	b.now = c.Time
	switch c.Kind {
	case CmdNew:
		b.newOrder(c)
	case CmdCancel:
		b.cancel(c)
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
	id := b.nextID
	b.nextID++
	b.emit(Event{Kind: EvAccepted, OrderID: id, Trader: c.Trader, Side: c.Side, Price: c.Price, Qty: c.Qty})

	t := taker{id: id, trader: c.Trader, side: c.Side, market: c.Type == Market, limit: c.Price, qty: c.Qty}
	b.match(&t)
	if t.qty == 0 {
		return
	}
	if t.market {
		b.emit(Event{Kind: EvCancelled, OrderID: id, Trader: c.Trader, Side: c.Side, Qty: t.qty, Reason: ReasonNoLiquidity})
		return
	}
	b.rest(&Order{ID: id, Trader: c.Trader, Price: c.Price, Qty: t.qty, Side: c.Side})
}

// match trades the taker against the opposite side, best price first and
// oldest order first within a price. Every trade happens at the maker's
// price. One BookDelta is emitted per level touched, after its fills.
func (b *Book) match(t *taker) {
	opp := b.opposite(t.side)
	makerSide := Sell
	if t.side == Sell {
		makerSide = Buy
	}
	for t.qty > 0 {
		lv := opp.best
		if lv == nil || !t.crosses(lv.price) {
			return
		}
		for t.qty > 0 && lv.head != nil {
			m := lv.head
			q := min(t.qty, m.Qty)
			m.Qty -= q
			lv.qty -= q
			t.qty -= q
			b.emit(Event{
				Kind: EvFill, Side: t.side, OrderID: t.id, Trader: t.trader,
				MakerOrderID: m.ID, MakerTrader: m.Trader,
				Price: lv.price, Qty: q, Remaining: t.qty, MakerRemaining: m.Qty,
			})
			if m.Qty == 0 {
				lv.unlink(m)
				delete(b.orders, m.ID)
			}
		}
		b.delta(makerSide, lv)
		if lv.head == nil {
			opp.remove(lv)
		}
	}
}

func (b *Book) rest(o *Order) {
	lv := b.side(o.Side).getOrCreate(o.Price)
	lv.pushBack(o)
	b.orders[o.ID] = o
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
	delete(b.orders, o.ID)
	b.delta(o.Side, lv)
	if lv.head == nil {
		s.remove(lv)
	}
}
