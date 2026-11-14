// Package book implements a single-symbol limit order book with price-time
// priority. A Book is a deterministic state machine: the same sequence of
// commands always produces the same sequence of events and the same state.
// It is not safe for concurrent use; one goroutine owns it.
package book

type Side uint8

const (
	Buy  Side = 1
	Sell Side = 2
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "buy"
	case Sell:
		return "sell"
	}
	return "unknown"
}

type OrderType uint8

const (
	Limit  OrderType = 1
	Market OrderType = 2
)

type TimeInForce uint8

const (
	GTC TimeInForce = 1
	IOC TimeInForce = 2
	FOK TimeInForce = 3
)

type CommandKind uint8

const (
	CmdNew    CommandKind = 1
	CmdCancel CommandKind = 2
	CmdAmend  CommandKind = 3
)

// Prices are integer ticks and quantities integer lots. The caps keep a
// level's aggregate quantity far away from int64 overflow.
const (
	MaxPrice = 1 << 40
	MaxQty   = 1 << 40
)

// Command is the only way to change a book. Time is stamped by the caller
// and copied onto the resulting events, so the book never reads a clock.
//
// For CmdNew the book assigns the order id. For CmdCancel and CmdAmend,
// OrderID names the target. In an amend, a zero Price or Qty means
// "leave unchanged" and Qty is the new open quantity.
type Command struct {
	Kind     CommandKind
	Side     Side
	Type     OrderType
	TIF      TimeInForce
	PostOnly bool
	Time     int64
	OrderID  uint64
	Trader   uint64
	Price    int64
	Qty      int64
}

type EventKind uint8

const (
	EvAccepted  EventKind = 1
	EvRejected  EventKind = 2
	EvFill      EventKind = 3
	EvCancelled EventKind = 4
	EvAmended   EventKind = 5
	EvBookDelta EventKind = 6
)

func (k EventKind) String() string {
	switch k {
	case EvAccepted:
		return "accepted"
	case EvRejected:
		return "rejected"
	case EvFill:
		return "fill"
	case EvCancelled:
		return "cancelled"
	case EvAmended:
		return "amended"
	case EvBookDelta:
		return "book_delta"
	}
	return "unknown"
}

// Reason qualifies a Rejected or Cancelled event.
type Reason uint8

const (
	ReasonNone Reason = iota
	ReasonInvalidCommand
	ReasonInvalidSide
	ReasonInvalidType
	ReasonInvalidTIF
	ReasonInvalidPrice
	ReasonInvalidQty
	ReasonInvalidTrader
	ReasonInvalidPostOnly
	ReasonUnknownOrder
	ReasonNotOwner
	ReasonWouldCross
	ReasonNoChange
	ReasonUserCancel
	ReasonIOC
	ReasonFOK
	ReasonNoLiquidity
	ReasonSelfTrade
)

var reasonNames = [...]string{
	ReasonNone:            "",
	ReasonInvalidCommand:  "invalid_command",
	ReasonInvalidSide:     "invalid_side",
	ReasonInvalidType:     "invalid_type",
	ReasonInvalidTIF:      "invalid_tif",
	ReasonInvalidPrice:    "invalid_price",
	ReasonInvalidQty:      "invalid_qty",
	ReasonInvalidTrader:   "invalid_trader",
	ReasonInvalidPostOnly: "invalid_post_only",
	ReasonUnknownOrder:    "unknown_order",
	ReasonNotOwner:        "not_owner",
	ReasonWouldCross:      "post_only_would_cross",
	ReasonNoChange:        "no_change",
	ReasonUserCancel:      "user_cancel",
	ReasonIOC:             "ioc_remainder",
	ReasonFOK:             "fok_unfillable",
	ReasonNoLiquidity:     "no_liquidity",
	ReasonSelfTrade:       "self_trade",
}

func (r Reason) String() string {
	if int(r) < len(reasonNames) {
		return reasonNames[r]
	}
	return "unknown"
}

// Event is a flat record so a slice of events is one contiguous allocation.
// Which fields are meaningful depends on Kind:
//
//	Accepted:  OrderID, Trader, Side, Price, Qty
//	Rejected:  OrderID (zero for a new order), Trader, Reason
//	Fill:      OrderID/Trader/Side are the taker's; MakerOrderID, MakerTrader,
//	           Price (the maker's), Qty, Remaining (taker), MakerRemaining
//	Cancelled: OrderID, Trader, Side, Price, Qty (amount removed), Reason
//	Amended:   OrderID, Trader, Side, Price, Qty (new open quantity)
//	BookDelta: Side, Price, Qty (new aggregate at that price; zero = level gone)
type Event struct {
	Seq            uint64
	Time           int64
	Kind           EventKind
	Side           Side
	Reason         Reason
	OrderID        uint64
	Trader         uint64
	MakerOrderID   uint64
	MakerTrader    uint64
	Price          int64
	Qty            int64
	Remaining      int64
	MakerRemaining int64
}

// Level is one aggregated price level of an L2 view.
type Level struct {
	Price int64
	Qty   int64
}
