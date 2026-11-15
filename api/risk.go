package api

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
)

// Risk holds the pre-trade checks applied to every order from outside. Bots
// run in-process and bypass them.
//
//   - Price band: a limit price must lie within CollarPct of the reference
//     price (the last trade, else the mid, else the symbol's configured
//     reference) and within StaticPct of the configured reference. The
//     dynamic collar stops fat fingers; the static band stops two accounts
//     from walking the last price away in collar-sized steps.
//   - Market orders are sent as IOC (or FOK) limits at the edge of the band,
//     so they can never sweep the book past it.
//   - Size: quantity at most MaxQty lots and notional at most MaxNotional.
//   - Resting GTC orders get a time to live of OrderTTL and are then expired
//     by the engine.
type Risk struct {
	CollarPct   float64
	StaticPct   float64
	MaxQty      int64
	MaxNotional float64
	OrderTTL    time.Duration
}

func (r *Risk) setDefaults() {
	if r.CollarPct <= 0 {
		r.CollarPct = 10
	}
	if r.StaticPct <= 0 {
		r.StaticPct = 25
	}
	if r.MaxQty <= 0 {
		r.MaxQty = 10_000
	}
	if r.MaxNotional <= 0 {
		r.MaxNotional = 5_000_000
	}
}

type band struct {
	BandLow  int64 `json:"band_low"`
	BandHigh int64 `json:"band_high"`
}

// band is the range of acceptable limit prices for a symbol right now.
func (s *Server) band(sym Symbol) band {
	tk := s.feeds[sym.Name].Ticker()
	ref := tk.Last
	if ref == 0 && tk.BestBid > 0 && tk.BestAsk > 0 {
		ref = (tk.BestBid + tk.BestAsk) / 2
	}
	if ref == 0 {
		ref = sym.Ref
	}
	if ref == 0 {
		return band{1, book.MaxPrice}
	}
	c := s.cfg.Risk.CollarPct / 100
	b := band{BandLow: int64(math.Ceil(float64(ref) * (1 - c))), BandHigh: int64(math.Floor(float64(ref) * (1 + c)))}
	if sym.Ref > 0 {
		st := s.cfg.Risk.StaticPct / 100
		b.BandLow = max(b.BandLow, int64(math.Ceil(float64(sym.Ref)*(1-st))))
		b.BandHigh = min(b.BandHigh, int64(math.Floor(float64(sym.Ref)*(1+st))))
	}
	b.BandLow = max(b.BandLow, 1)
	return b
}

// riskError is a pre-trade rejection, reported as 422 with its code.
type riskError struct{ code, msg string }

func (s *Server) symbol(name string) (Symbol, bool) {
	for _, sym := range s.cfg.Symbols {
		if sym.Name == name {
			return sym, true
		}
	}
	return Symbol{}, false
}

// checkNew applies the risk checks to a new order, rewriting market orders
// into band-limited IOC/FOK orders and stamping GTC orders with a TTL.
func (s *Server) checkNew(sym Symbol, c *book.Command) *riskError {
	rk := &s.cfg.Risk
	b := s.band(sym)
	if c.Type == book.Market {
		c.Type, c.Price = book.Limit, b.BandHigh
		if c.Side == book.Sell {
			c.Price = b.BandLow
		}
	} else if c.Price < b.BandLow || c.Price > b.BandHigh {
		return &riskError{"price_outside_band", "price is outside the allowed band"}
	}
	if err := s.checkSize(sym, c.Price, c.Qty); err != nil {
		return err
	}
	if c.TIF == book.GTC && rk.OrderTTL > 0 {
		c.TTL = int64(rk.OrderTTL)
	}
	return nil
}

func (s *Server) checkSize(sym Symbol, price, qty int64) *riskError {
	rk := &s.cfg.Risk
	if qty > rk.MaxQty {
		return &riskError{"qty_above_max", "quantity is above the per-order maximum"}
	}
	if float64(price)/math.Pow10(sym.PriceScale)*float64(qty) > rk.MaxNotional {
		return &riskError{"notional_above_max", "order value is above the per-order maximum"}
	}
	return nil
}

// checkAmend checks an amend against the band and size limits, looking up
// the order for whichever of price and quantity the amend leaves unchanged.
func (s *Server) checkAmend(ctx context.Context, e *engine.Engine, c book.Command) (*riskError, error) {
	sym, _ := s.symbol(e.Symbol())
	price, total := c.Price, c.Qty
	if price == 0 || total == 0 {
		var found bool
		err := e.Query(ctx, func(b *book.Book) {
			if o := b.Order(c.OrderID); o != nil {
				found = true
				price = cmp(price, o.Price)
				total = cmp(total, o.Qty+o.Filled)
			}
		})
		if err != nil || !found {
			return nil, err
		}
	}
	if c.Price != 0 {
		if b := s.band(sym); c.Price < b.BandLow || c.Price > b.BandHigh {
			return &riskError{"price_outside_band", "price is outside the allowed band"}, nil
		}
	}
	return s.checkSize(sym, price, total), nil
}

// cmp returns v unless it is zero, in which case it returns def.
func cmp(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func writeRisk(w http.ResponseWriter, e *riskError) {
	writeError(w, http.StatusUnprocessableEntity, e.code, e.msg)
}
