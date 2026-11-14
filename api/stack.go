package api

import (
	"context"
	"sync"

	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/feed"
)

// Stack is a running exchange: engines, their feeds and the gateway.
type Stack struct {
	Exchange *engine.Exchange
	Feeds    map[string]*feed.Feed
	Server   *Server

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start opens (and recovers) one engine per symbol, wires each to a feed and
// starts everything. configure may adjust each engine's config, for example
// to give it a data directory; it must not replace Events.
func Start(cfg Config, feedBuffer int, configure func(*engine.Config)) (*Stack, error) {
	names := make([]string, len(cfg.Symbols))
	feeds := make(map[string]*feed.Feed, len(cfg.Symbols))
	for i, s := range cfg.Symbols {
		names[i] = s.Name
		feeds[s.Name] = feed.New(s.Name, feedBuffer)
	}
	x, err := engine.OpenExchange(names, func(c *engine.Config) {
		if configure != nil {
			configure(c)
		}
		c.Events = feeds[c.Symbol].Publish
	})
	if err != nil {
		return nil, err
	}
	x.Start()
	ctx, cancel := context.WithCancel(context.Background())
	st := &Stack{Exchange: x, Feeds: feeds, cancel: cancel}
	for _, e := range x.Engines() {
		f := feeds[e.Symbol()]
		st.wg.Go(func() { f.Run(ctx, State(e)) })
	}
	st.Server = New(cfg, x, feeds)
	st.wg.Go(func() { st.Server.Run(ctx) })
	return st, nil
}

// Close stops the engines first, so their final events reach the feeds,
// then the feeds and the stats sampler.
func (s *Stack) Close() error {
	err := s.Exchange.Close()
	s.cancel()
	s.wg.Wait()
	return err
}
