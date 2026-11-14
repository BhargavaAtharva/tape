package engine

import (
	"errors"
	"fmt"
)

// idShift leaves 40 bits for the per-symbol order counter. With the symbol
// number above it, ids for up to 8191 symbols stay below 2^53 and survive a
// round trip through a JSON number.
const (
	idShift    = 40
	MaxSymbols = 1<<13 - 1
)

// Exchange routes commands to per-symbol engines. The symbol list is fixed at
// construction, so lookups need no synchronisation.
type Exchange struct {
	engines []*Engine
	byName  map[string]*Engine
}

// OpenExchange opens one engine per symbol. A symbol's position in the list
// becomes part of its order ids, so new symbols must be appended, never
// inserted, once a data directory exists. configure may adjust each engine's
// config before it is opened.
func OpenExchange(symbols []string, configure func(*Config)) (*Exchange, error) {
	if len(symbols) == 0 {
		return nil, errors.New("engine: no symbols configured")
	}
	if len(symbols) > MaxSymbols {
		return nil, fmt.Errorf("engine: %d symbols exceeds the limit of %d", len(symbols), MaxSymbols)
	}
	x := &Exchange{byName: make(map[string]*Engine, len(symbols))}
	for i, s := range symbols {
		if !validSymbol(s) || x.byName[s] != nil {
			x.Close()
			return nil, fmt.Errorf("engine: invalid or duplicate symbol %q", s)
		}
		cfg := Config{Symbol: s, IDBase: uint64(i+1) << idShift}
		if configure != nil {
			configure(&cfg)
		}
		e, err := Open(cfg)
		if err != nil {
			x.Close()
			return nil, fmt.Errorf("engine: open %s: %w", s, err)
		}
		x.engines = append(x.engines, e)
		x.byName[s] = e
	}
	return x, nil
}

func (x *Exchange) Start() {
	for _, e := range x.engines {
		e.Start()
	}
}

func (x *Exchange) Engines() []*Engine { return x.engines }

func (x *Exchange) Engine(symbol string) (*Engine, bool) {
	e, ok := x.byName[symbol]
	return e, ok
}

// ForOrder finds the engine that issued an order id.
func (x *Exchange) ForOrder(id uint64) (*Engine, bool) {
	i := id >> idShift
	if i == 0 || i > uint64(len(x.engines)) {
		return nil, false
	}
	return x.engines[i-1], true
}

func (x *Exchange) Close() error {
	var first error
	for _, e := range x.engines {
		if err := e.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// validSymbol keeps symbols short and safe to use as a directory name and in
// a URL path: 1 to 16 characters from A-Z, 0-9, '-' and '.', not starting
// with '.'.
func validSymbol(s string) bool {
	if len(s) == 0 || len(s) > 16 || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
