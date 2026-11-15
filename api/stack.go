package api

import (
	"context"
	"crypto/rand"
	"sync"

	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/feed"
)

// Stack is a running exchange: engines, their feeds and the gateway.
type Stack struct {
	Exchange *engine.Exchange
	Feeds    map[string]*feed.Feed
	Reports  *feed.Reports
	Server   *Server

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start opens (and recovers) one engine per symbol, wires each to a feed and
// starts everything. configure may adjust each engine's config, for example
// to give it a data directory; it must not replace Events. Every feed gets
// fo, plus the stack's epoch and private report hub.
func Start(cfg Config, fo feed.Options, configure func(*engine.Config)) (*Stack, error) {
	names := make([]string, len(cfg.Symbols))
	feeds := make(map[string]*feed.Feed, len(cfg.Symbols))
	reports := feed.NewReports(fo.Buffer)
	fo.Reports = reports
	fo.Epoch = rand.Text()[:12]
	for i, s := range cfg.Symbols {
		names[i] = s.Name
		feeds[s.Name] = feed.New(s.Name, fo)
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
	st := &Stack{Exchange: x, Feeds: feeds, Reports: reports, cancel: cancel}
	for _, e := range x.Engines() {
		f := feeds[e.Symbol()]
		st.wg.Go(func() { f.Run(ctx, State(e)) })
	}
	st.Server = New(cfg, x, feeds, reports)
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
