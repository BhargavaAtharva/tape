// Package sim runs bot traders against the exchange so a demo instance has
// a live market. Each symbol gets one goroutine that moves a hidden fair
// price along a mean-reverting random walk and acts for every bot in turn:
//
//   - market makers keep a ladder of quotes either side of the fair price,
//     amending them as it moves;
//   - a depth provider rests orders further out and cancels the stale ones;
//   - noise takers send small market and IOC orders at random;
//   - momentum takers buy after the price has risen and sell after it fell.
//
// Bots submit commands through the same engine API as everyone else, so
// everything they do is logged, matched and published like any other order.
package sim

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
)

// Bot trader ids are 1..MaxBotTrader. The gateway refuses them to outside
// clients so nobody can cancel a bot's quotes.
const MaxBotTrader = 999

const (
	step         = 100 * time.Millisecond
	makers       = 3
	makerLevels  = 6
	depthTrader  = 10
	noiseFirst   = 20
	noiseCount   = 4
	momentumID   = 30
	reversion    = 0.02  // per second, toward the reference price
	volatility   = 0.002 // per square-root second, in log price
	momentumLook = 50    // steps
)

type Market struct {
	Symbol string
	// Ref is the long-run fair price in ticks.
	Ref int64
}

type Config struct {
	Markets []Market
	// Intensity scales how often takers trade and makers requote. At 1 each
	// symbol sees a few dozen commands a second.
	Intensity float64
	Seed      uint64
}

// Run drives every market until ctx is done.
func Run(ctx context.Context, x *engine.Exchange, cfg Config) error {
	if cfg.Intensity <= 0 {
		cfg.Intensity = 1
	}
	markets := make([]*market, len(cfg.Markets))
	for i, m := range cfg.Markets {
		e, ok := x.Engine(m.Symbol)
		if !ok {
			return errors.New("sim: unknown symbol " + m.Symbol)
		}
		if m.Ref <= 0 {
			return errors.New("sim: reference price must be positive for " + m.Symbol)
		}
		markets[i] = newMarket(e, m.Ref, cfg.Intensity, cfg.Seed+uint64(i))
	}
	errc := make(chan error, len(markets))
	for _, mk := range markets {
		go func() { errc <- mk.run(ctx) }()
	}
	var first error
	for range cfg.Markets {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

type slot struct {
	side  book.Side
	level int
}

type market struct {
	e         *engine.Engine
	ref       float64
	logFair   float64
	history   []float64
	intensity float64
	rng       *rand.Rand
	// tick is the price granularity the bots quote in: about a basis point
	// of the reference price, so ladders look sensible at any price.
	tick int64
	// quotes maps each maker's ladder slot to its resting order.
	quotes [makers + 1]map[slot]uint64
	depth  []uint64
}

func newMarket(e *engine.Engine, ref int64, intensity float64, seed uint64) *market {
	m := &market{
		e: e, ref: float64(ref), intensity: intensity,
		rng:  rand.New(rand.NewPCG(seed, uint64(time.Now().UnixNano()))),
		tick: max(1, ref/10_000),
	}
	for i := range m.quotes {
		m.quotes[i] = make(map[slot]uint64)
	}
	return m
}

func (m *market) fair() int64 { return int64(m.ref * math.Exp(m.logFair)) }

func (m *market) run(ctx context.Context) error {
	if err := m.adopt(ctx); err != nil {
		return err
	}
	t := time.NewTicker(step)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		m.walk()
		if err := m.act(ctx); err != nil {
			if ctx.Err() != nil || errors.Is(err, engine.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

// adopt picks up where a previous run left off: the fair price starts at
// the current mid, and resting bot orders from before a restart are
// cancelled so makers start from clean ladders.
func (m *market) adopt(ctx context.Context) error {
	var stale []book.Command
	var bid, ask int64
	err := m.e.Query(ctx, func(b *book.Book) {
		bid, _, _ = b.BestBid()
		ask, _, _ = b.BestAsk()
		for _, s := range []book.Side{book.Buy, book.Sell} {
			b.Orders(s, func(o *book.Order) {
				if o.Trader <= MaxBotTrader {
					stale = append(stale, book.Command{Kind: book.CmdCancel, Trader: o.Trader, OrderID: o.ID})
				}
			})
		}
	})
	if err != nil {
		return err
	}
	if bid > 0 && ask > 0 {
		m.logFair = math.Log(float64(bid+ask) / 2 / m.ref)
	}
	for _, c := range stale {
		if _, err := m.e.Submit(ctx, c); err != nil {
			return err
		}
	}
	if len(stale) > 0 {
		slog.Info("sim adopted book", "symbol", m.e.Symbol(), "cancelled", len(stale))
	}
	return nil
}

// walk advances the fair price one step of an Ornstein-Uhlenbeck process in
// log space: it wanders but is always pulled back toward the reference.
func (m *market) walk() {
	dt := step.Seconds()
	m.logFair += -reversion*m.logFair*dt + volatility*math.Sqrt(dt)*m.rng.NormFloat64()
	m.history = append(m.history, m.logFair)
	if len(m.history) > momentumLook {
		m.history = m.history[1:]
	}
}

// chance reports whether an event with the given rate per second happens in
// this step.
func (m *market) chance(perSecond float64) bool {
	return m.rng.Float64() < perSecond*m.intensity*step.Seconds()
}

func (m *market) act(ctx context.Context) error {
	for mk := 1; mk <= makers; mk++ {
		if m.chance(2) {
			if err := m.requote(ctx, uint64(mk)); err != nil {
				return err
			}
		}
	}
	if m.chance(3) {
		if err := m.addDepth(ctx); err != nil {
			return err
		}
	}
	for n := range noiseCount {
		if m.chance(1.5) {
			if err := m.noise(ctx, uint64(noiseFirst+n)); err != nil {
				return err
			}
		}
	}
	if m.chance(1) {
		return m.momentum(ctx)
	}
	return nil
}

func (m *market) submit(ctx context.Context, c book.Command) ([]book.Event, error) {
	res, err := m.e.Submit(ctx, c)
	return res.Events, err
}

// requote moves one maker's ladder to the current fair price. Makers differ
// in how wide they quote, which gives the book some texture.
func (m *market) requote(ctx context.Context, trader uint64) error {
	open, err := m.openOrders(ctx, trader)
	if err != nil {
		return err
	}
	fair := m.fair()
	half := m.tick * int64(1+trader)
	for lvl := range makerLevels {
		for _, side := range []book.Side{book.Buy, book.Sell} {
			sl := slot{side, lvl}
			price := fair - half - int64(lvl)*m.tick*int64(trader)
			if side == book.Sell {
				price = fair + half + int64(lvl)*m.tick*int64(trader)
			}
			price = max(price, 1)
			qty := 5 + m.rng.Int64N(20)*int64(lvl+1)
			id, ok := m.quotes[trader][sl]
			cur, live := open[id]
			switch {
			case ok && live && cur.Price == price && cur.Qty*2 >= qty:
				continue
			case ok && live:
				evs, err := m.submit(ctx, book.Command{Kind: book.CmdAmend, Trader: trader, OrderID: id, Price: price, Qty: qty})
				if err != nil {
					return err
				}
				if gone(evs, id) {
					delete(m.quotes[trader], sl)
				}
			default:
				evs, err := m.submit(ctx, book.Command{Kind: book.CmdNew, Type: book.Limit, TIF: book.GTC, PostOnly: true,
					Side: side, Trader: trader, Price: price, Qty: qty})
				if err != nil {
					return err
				}
				if id := restingID(evs); id != 0 {
					m.quotes[trader][sl] = id
				} else {
					delete(m.quotes[trader], sl)
				}
			}
		}
	}
	return nil
}

func (m *market) openOrders(ctx context.Context, trader uint64) (map[uint64]book.Order, error) {
	open := make(map[uint64]book.Order)
	err := m.e.Query(ctx, func(b *book.Book) {
		for _, id := range m.quotes[trader] {
			if o := b.Order(id); o != nil {
				open[id] = *o
			}
		}
	})
	return open, err
}

// gone reports whether an amend left the order out of the book: rejected
// because it had already traded away, or filled or cancelled on re-entry.
func gone(evs []book.Event, id uint64) bool {
	for _, e := range evs {
		switch {
		case e.Kind == book.EvRejected:
			return e.Reason == book.ReasonUnknownOrder
		case e.Kind == book.EvCancelled && e.OrderID == id:
			return true
		case e.Kind == book.EvFill && e.OrderID == id && e.Remaining == 0:
			return true
		}
	}
	return false
}

func restingID(evs []book.Event) uint64 {
	if len(evs) == 0 || evs[0].Kind != book.EvAccepted {
		return 0
	}
	id, open := evs[0].OrderID, evs[0].Qty
	for _, e := range evs[1:] {
		if e.OrderID == id && (e.Kind == book.EvFill || e.Kind == book.EvCancelled) {
			open -= e.Qty
		}
	}
	if open > 0 {
		return id
	}
	return 0
}

// addDepth rests an order well away from the touch and, once enough have
// accumulated, cancels the oldest.
func (m *market) addDepth(ctx context.Context) error {
	side := book.Side(1 + m.rng.IntN(2))
	off := m.tick * (8 + m.rng.Int64N(40))
	price := m.fair() - off
	if side == book.Sell {
		price = m.fair() + off
	}
	evs, err := m.submit(ctx, book.Command{Kind: book.CmdNew, Type: book.Limit, TIF: book.GTC,
		Side: side, Trader: depthTrader, Price: max(price, 1), Qty: 10 + m.rng.Int64N(90)})
	if err != nil {
		return err
	}
	if id := restingID(evs); id != 0 {
		m.depth = append(m.depth, id)
	}
	if len(m.depth) > 60 {
		id := m.depth[0]
		m.depth = m.depth[1:]
		_, err = m.submit(ctx, book.Command{Kind: book.CmdCancel, Trader: depthTrader, OrderID: id})
	}
	return err
}

func (m *market) noise(ctx context.Context, trader uint64) error {
	side := book.Side(1 + m.rng.IntN(2))
	qty := 1 + m.rng.Int64N(12)
	c := book.Command{Kind: book.CmdNew, Type: book.Market, TIF: book.IOC, Side: side, Trader: trader, Qty: qty}
	if m.rng.IntN(3) == 0 {
		// An IOC limit a couple of ticks through the fair price.
		c.Type, c.Price = book.Limit, m.fair()+2*m.tick
		if side == book.Sell {
			c.Price = max(m.fair()-2*m.tick, 1)
		}
	}
	_, err := m.submit(ctx, c)
	return err
}

func (m *market) momentum(ctx context.Context) error {
	if len(m.history) < momentumLook {
		return nil
	}
	move := m.history[len(m.history)-1] - m.history[0]
	if math.Abs(move) < 0.001 {
		return nil
	}
	side := book.Buy
	if move < 0 {
		side = book.Sell
	}
	_, err := m.submit(ctx, book.Command{Kind: book.CmdNew, Type: book.Market, TIF: book.IOC,
		Side: side, Trader: momentumID, Qty: 5 + m.rng.Int64N(20)})
	return err
}
