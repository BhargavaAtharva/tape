package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/feed"
)

type testStack struct {
	*Stack
	url string
}

func startStack(t *testing.T, cfg Config, feedBuffer int, configure func(*engine.Config)) *testStack {
	t.Helper()
	if cfg.Symbols == nil {
		cfg.Symbols = []Symbol{{Name: "BTC-USD", PriceScale: 2}, {Name: "ETH-USD", PriceScale: 2}}
	}
	if cfg.Origins == nil {
		cfg.Origins = []string{"*"}
	}
	st, err := Start(cfg, feed.Options{Buffer: feedBuffer}, configure)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(st.Server)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return &testStack{Stack: st, url: srv.URL}
}

func (s *testStack) do(t *testing.T, method, path string, trader uint64, body any) (int, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	if trader != 0 {
		req.Header.Set("X-Trader-ID", fmt.Sprint(trader))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (s *testStack) order(t *testing.T, method, path string, trader uint64, body any, wantStatus int) orderResult {
	t.Helper()
	code, raw := s.do(t, method, path, trader, body)
	if code != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, wantStatus, raw)
	}
	var res orderResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, raw)
	}
	return res
}

func TestOrderLifecycleOverREST(t *testing.T) {
	s := startStack(t, Config{}, 0, nil)

	ask := s.order(t, "POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "sell", "price": 10100, "qty": 5}, 200)
	if ask.Status != "resting" || ask.Remaining != 5 || ask.OrderID == 0 {
		t.Fatalf("ask %+v", ask)
	}
	bid := s.order(t, "POST", "/v1/orders", 2, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 10100, "qty": 3, "tif": "ioc"}, 200)
	if bid.Status != "filled" || bid.Filled != 3 {
		t.Fatalf("crossing bid %+v", bid)
	}
	mkt := s.order(t, "POST", "/v1/orders", 2, map[string]any{"symbol": "BTC-USD", "side": "buy", "type": "market", "qty": 4}, 200)
	if mkt.Status != "cancelled" || mkt.Filled != 2 || mkt.Reason != "ioc_remainder" {
		t.Fatalf("market order %+v", mkt)
	}
	rej := s.order(t, "POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 10000, "qty": 0}, 422)
	if rej.Status != "rejected" || rej.Reason != "invalid_qty" {
		t.Fatalf("zero qty %+v", rej)
	}

	rest := s.order(t, "POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 9900, "qty": 10}, 200)
	path := fmt.Sprintf("/v1/orders/%d", rest.OrderID)
	s.order(t, "DELETE", path, 2, nil, 403)
	am := s.order(t, "PATCH", path, 1, map[string]any{"qty": 4}, 200)
	if am.Status != "resting" || am.Remaining != 4 {
		t.Fatalf("amend %+v", am)
	}
	s.order(t, "PATCH", path, 1, map[string]any{"qty": 4}, 422)

	code, raw := s.do(t, "GET", "/v1/orders?symbol=BTC-USD", 1, nil)
	var open []openOrder
	json.Unmarshal(raw, &open)
	if code != 200 || len(open) != 1 || open[0].OrderID != rest.OrderID || open[0].Qty != 4 {
		t.Fatalf("open orders %d %s", code, raw)
	}

	c := s.order(t, "DELETE", path, 1, nil, 200)
	if c.Status != "cancelled" || c.Reason != "user_cancel" {
		t.Fatalf("cancel %+v", c)
	}
	s.order(t, "DELETE", path, 1, nil, 404)
	s.order(t, "DELETE", "/v1/orders/12345", 1, nil, 404)

	waitFor(t, func() bool { return len(s.Feeds["BTC-USD"].Trades(10)) == 2 })
	code, raw = s.do(t, "GET", "/v1/trades/BTC-USD?limit=10", 0, nil)
	var trades []feed.Trade
	json.Unmarshal(raw, &trades)
	if code != 200 || len(trades) != 2 || trades[0].Qty != 3 || trades[1].Qty != 2 || trades[0].MakerOrderID != ask.OrderID {
		t.Fatalf("trades %d %s", code, raw)
	}

	code, raw = s.do(t, "GET", "/v1/symbols", 0, nil)
	if code != 200 || !strings.Contains(string(raw), `"symbol":"BTC-USD","price_scale":2,"reference":0,"last":10100`) {
		t.Fatalf("symbols %d %s", code, raw)
	}
}

func TestRequestValidation(t *testing.T) {
	s := startStack(t, Config{}, 0, nil)
	for _, tc := range []struct {
		method, path string
		trader       uint64
		body         any
		want         int
		code         string
	}{
		{"POST", "/v1/orders", 0, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 1, "qty": 1}, 401, "invalid_trader"},
		{"POST", "/v1/orders", 1 << 60, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 1, "qty": 1}, 401, "invalid_trader"},
		{"POST", "/v1/orders", 1, map[string]any{"symbol": "DOGE", "side": "buy", "price": 1, "qty": 1}, 404, "unknown_symbol"},
		{"POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "up", "price": 1, "qty": 1}, 400, "invalid_order"},
		{"POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "tif": "day", "price": 1, "qty": 1}, 400, "invalid_order"},
		{"POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 1.5, "qty": 1}, 400, "invalid_body"},
		{"POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 1, "qty": 1, "extra": 1}, 400, "invalid_body"},
		{"PATCH", "/v1/orders/abc", 1, map[string]any{"qty": 1}, 400, "invalid_order_id"},
		{"GET", "/v1/book/NOPE", 0, nil, 404, "unknown_symbol"},
		{"GET", "/v1/book/BTC-USD?depth=0", 0, nil, 400, "invalid_depth"},
		{"GET", "/v1/trades/BTC-USD?limit=x", 0, nil, 400, "invalid_limit"},
	} {
		code, raw := s.do(t, tc.method, tc.path, tc.trader, tc.body)
		var e apiError
		json.Unmarshal(raw, &e)
		if code != tc.want || e.Error != tc.code {
			t.Errorf("%s %s: %d %s, want %d %s", tc.method, tc.path, code, raw, tc.want, tc.code)
		}
	}
}

func TestCORSPreflightAndRateLimit(t *testing.T) {
	s := startStack(t, Config{Origins: []string{"https://tape.example"}, RateLimit: 5}, 0, nil)
	req, _ := http.NewRequest("OPTIONS", s.url+"/v1/orders", nil)
	req.Header.Set("Origin", "https://tape.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://tape.example" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "X-Trader-ID") {
		t.Fatalf("preflight %d %v", resp.StatusCode, resp.Header)
	}

	limited := 0
	for range 20 {
		code, _ := s.do(t, "POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 100, "qty": 1})
		if code == 429 {
			limited++
		}
	}
	if limited < 5 {
		t.Fatalf("only %d of 20 burst requests were limited", limited)
	}
	if code, _ := s.do(t, "GET", "/v1/book/BTC-USD", 0, nil); code != 200 {
		t.Fatalf("reads are rate limited: %d", code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// wsClient keeps a local L2 book from the feed and checks the sequence
// chain on every message.
type wsClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
	seq  uint64
	msgs int
	bids map[int64]int64
	asks map[int64]int64
	err  error
}

func dialWS(t *testing.T, url string, client *http.Client) *websocket.Conn {
	t.Helper()
	ws := "ws" + strings.TrimPrefix(url, "http")
	conn, _, err := websocket.Dial(context.Background(), ws, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(1 << 22)
	return conn
}

func (c *wsClient) run() {
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		if err := c.apply(data); err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
	}
}

func (c *wsClient) apply(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var head struct{ Type string }
	json.Unmarshal(data, &head)
	c.msgs++
	switch head.Type {
	case "snapshot":
		var s feed.Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if c.msgs != 1 {
			return errors.New("snapshot after the first message")
		}
		c.seq = s.Seq
		c.bids, c.asks = make(map[int64]int64), make(map[int64]int64)
		for _, l := range s.Bids {
			c.bids[l[0]] = l[1]
		}
		for _, l := range s.Asks {
			c.asks[l[0]] = l[1]
		}
	case "update":
		var u feed.Update
		if err := json.Unmarshal(data, &u); err != nil {
			return err
		}
		if u.Prev != c.seq || u.Seq <= u.Prev {
			return fmt.Errorf("sequence gap: update %d..%d after %d", u.Prev, u.Seq, c.seq)
		}
		c.seq = u.Seq
		for _, d := range u.Deltas {
			m := c.asks
			if d.Side == "buy" {
				m = c.bids
			}
			if d.Qty == 0 {
				delete(m, d.Price)
			} else {
				m[d.Price] = d.Qty
			}
		}
	default:
		return fmt.Errorf("unknown message %s", data)
	}
	return nil
}

// TestSlowConsumerIsDroppedOthersUnaffected connects several clients that
// read normally and one whose socket receive buffer is tiny and that never
// reads. Under sustained traffic the slow one must be cut off with a policy
// violation while every other client receives an unbroken sequence and ends
// with exactly the server's book.
func TestSlowConsumerIsDroppedOthersUnaffected(t *testing.T) {
	s := startStack(t, Config{WriteTimeout: 300 * time.Millisecond}, 1024, nil)
	wsURL := s.url + "/v1/ws/BTC-USD?depth=1000"

	tinyBuffer := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{
		Control: func(_, _ string, rc syscall.RawConn) error {
			return rc.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1024) })
		},
	}).DialContext}}
	slow := dialWS(t, wsURL, tinyBuffer)
	defer slow.CloseNow()

	var fast []*wsClient
	for range 3 {
		c := &wsClient{conn: dialWS(t, wsURL, nil)}
		defer c.conn.CloseNow()
		go c.run()
		fast = append(fast, c)
	}
	waitFor(t, func() bool { return s.Feeds["BTC-USD"].Stats().Subscribers == 4 })

	// Traffic runs until the slow client has been dropped, paced so that the
	// clients that do read keep up even under the race detector.
	e, _ := s.Exchange.Engine("BTC-USD")
	btc := s.Feeds["BTC-USD"]
	var wg sync.WaitGroup
	stop := time.Now().Add(30 * time.Second)
	for w := range 4 {
		wg.Go(func() {
			ctx := context.Background()
			for i := 0; btc.Stats().Dropped == 0 && time.Now().Before(stop); i++ {
				if i%50 == 0 {
					time.Sleep(time.Millisecond)
				}
				side := book.Buy
				price := int64(10000 - 1 - (i*7+w)%400)
				if (i+w)%2 == 0 {
					side, price = book.Sell, int64(10000+(i*11+w)%400)
				}
				if i%10 == 9 {
					price = 10000
				}
				c := book.Command{Kind: book.CmdNew, Type: book.Limit, TIF: book.GTC, Trader: uint64(1 + w + 4*(i%250)), Side: side, Price: price, Qty: int64(1 + i%30)}
				if _, err := e.Submit(ctx, c); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if st := btc.Stats(); st.Dropped != 1 || st.Subscribers != 3 {
		t.Fatalf("feed stats %+v, want exactly the slow client dropped", st)
	}

	// The slow client drains what was already in flight and then finds the
	// connection gone. A client this stuck cannot be sent a close frame, so
	// the server closes the socket after the close handshake times out.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = slow.Read(ctx)
	}
	if ctx.Err() != nil {
		t.Fatal("slow client connection was never closed")
	}

	want := s.Feeds["BTC-USD"].Book(0)
	for i, c := range fast {
		waitFor(t, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.err != nil || c.seq == want.Seq
		})
		c.mu.Lock()
		if c.err != nil {
			t.Fatalf("client %d: %v", i, c.err)
		}
		if len(c.bids) != len(want.Bids) || len(c.asks) != len(want.Asks) {
			t.Fatalf("client %d: %d/%d levels, server has %d/%d", i, len(c.bids), len(c.asks), len(want.Bids), len(want.Asks))
		}
		for _, l := range want.Bids {
			if c.bids[l[0]] != l[1] {
				t.Fatalf("client %d: bid %d is %d, server says %d", i, l[0], c.bids[l[0]], l[1])
			}
		}
		for _, l := range want.Asks {
			if c.asks[l[0]] != l[1] {
				t.Fatalf("client %d: ask %d is %d, server says %d", i, l[0], c.asks[l[0]], l[1])
			}
		}
		t.Logf("client %d: %d messages, seq %d", i, c.msgs, c.seq)
		c.mu.Unlock()
	}
}

func TestBookAndStatsEndpoints(t *testing.T) {
	s := startStack(t, Config{Info: map[string]string{"wal_sync": "batch"}}, 0, nil)
	for i := range 5 {
		s.order(t, "POST", "/v1/orders", 1, map[string]any{"symbol": "ETH-USD", "side": "buy", "price": 2000 - i, "qty": 1 + i}, 200)
	}
	waitFor(t, func() bool { return len(s.Feeds["ETH-USD"].Book(0).Bids) == 5 })
	code, raw := s.do(t, "GET", "/v1/book/ETH-USD?depth=3", 0, nil)
	var b feed.Snapshot
	json.Unmarshal(raw, &b)
	if code != 200 || len(b.Bids) != 3 || b.Bids[0] != [2]int64{2000, 1} || b.Bids[2] != [2]int64{1998, 3} || len(b.Asks) != 0 {
		t.Fatalf("book %d %s", code, raw)
	}
	s.Server.stats.sample(time.Now())
	code, raw = s.do(t, "GET", "/v1/stats", 0, nil)
	var st Stats
	json.Unmarshal(raw, &st)
	if code != 200 || st.Commands != 5 || len(st.Symbols) != 2 || st.Info["wal_sync"] != "batch" || st.LatencyNs.P99 == 0 {
		t.Fatalf("stats %d %s", code, raw)
	}
}

func TestBookSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	durable := func(c *engine.Config) { c.Dir = dir + "/" + c.Symbol }
	cfg := Config{Symbols: []Symbol{{Name: "BTC-USD", PriceScale: 2}}, Origins: []string{"*"}}
	st, err := Start(cfg, feed.Options{}, durable)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(st.Server)
	s := &testStack{Stack: st, url: srv.URL}
	s.order(t, "POST", "/v1/orders", 1, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 500, "qty": 7}, 200)
	s.order(t, "POST", "/v1/orders", 2, map[string]any{"symbol": "BTC-USD", "side": "sell", "price": 510, "qty": 2}, 200)
	srv.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	s = startStack(t, cfg, 0, durable)
	waitFor(t, func() bool { return s.Feeds["BTC-USD"].Book(0).Seq == 4 })
	code, raw := s.do(t, "GET", "/v1/book/BTC-USD", 0, nil)
	if code != 200 || !strings.Contains(string(raw), `"bids":[[500,7]],"asks":[[510,2]]`) {
		t.Fatalf("book after restart: %d %s", code, raw)
	}
}

func TestPrivateExecutionReports(t *testing.T) {
	s := startStack(t, Config{MinTrader: 1000}, 0, nil)
	ws := "ws" + strings.TrimPrefix(s.url, "http") + "/v1/ws/private?trader=5001"
	conn, _, err := websocket.Dial(context.Background(), ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	waitFor(t, func() bool { return s.Reports.Subscribers() == 1 })

	ask := s.order(t, "POST", "/v1/orders", 5001, map[string]any{"symbol": "BTC-USD", "side": "sell", "price": 100, "qty": 5}, 200)
	s.order(t, "POST", "/v1/orders", 5002, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 100, "qty": 2}, 200)

	var got []feed.Report
	for len(got) < 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("after %d reports: %v", len(got), err)
		}
		var r feed.Report
		json.Unmarshal(data, &r)
		got = append(got, r)
	}
	if got[0].Kind != "accepted" || got[0].OrderID != ask.OrderID {
		t.Fatalf("ack %+v", got[0])
	}
	if got[1].Kind != "fill" || got[1].OrderID != ask.OrderID || got[1].Remaining != 3 || got[1].Liquidity != "maker" {
		t.Fatalf("fill %+v", got[1])
	}
	if _, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(s.url, "http")+"/v1/ws/private?trader=5", nil); err == nil {
		t.Fatal("a bot trader id was accepted on the private stream")
	}
}

func TestWebSocketConnectionCap(t *testing.T) {
	s := startStack(t, Config{MaxConnsPerAddr: 2}, 0, nil)
	url := s.url + "/v1/ws/BTC-USD"
	for range 2 {
		c := dialWS(t, url, nil)
		defer c.CloseNow()
	}
	_, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(url, "http"), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third connection from one address: %v", err)
	}
}

func TestPreTradeRiskChecks(t *testing.T) {
	cfg := Config{
		Symbols: []Symbol{{Name: "BTC-USD", PriceScale: 2, Ref: 10_000}},
		Risk:    Risk{CollarPct: 10, StaticPct: 25, MaxQty: 100, MaxNotional: 5_000, OrderTTL: time.Minute},
	}
	s := startStack(t, cfg, 0, nil)
	post := func(trader uint64, body map[string]any, want int) (orderResult, apiError) {
		t.Helper()
		body["symbol"] = "BTC-USD"
		code, raw := s.do(t, "POST", "/v1/orders", trader, body)
		if code != want {
			t.Fatalf("%v: status %d, want %d: %s", body, code, want, raw)
		}
		var res orderResult
		var e apiError
		json.Unmarshal(raw, &res)
		json.Unmarshal(raw, &e)
		return res, e
	}

	// The attack from the review: a huge buy that would sweep the book.
	if _, e := post(5000, map[string]any{"side": "buy", "price": 1 << 40, "qty": 1 << 40}, 422); e.Error != "price_outside_band" {
		t.Fatalf("huge price: %+v", e)
	}
	if _, e := post(5000, map[string]any{"side": "buy", "price": 10_000, "qty": 101}, 422); e.Error != "qty_above_max" {
		t.Fatalf("huge qty: %+v", e)
	}
	if _, e := post(5000, map[string]any{"side": "buy", "price": 10_000, "qty": 60}, 422); e.Error != "notional_above_max" {
		t.Fatalf("notional 6000: %+v", e)
	}

	// The band follows the last trade. An ask placed while the price was
	// high must be out of reach of a market buy once the price has fallen.
	trade := func(price int) {
		post(5001, map[string]any{"side": "sell", "price": price, "qty": 1}, 200)
		post(5002, map[string]any{"side": "buy", "price": price, "qty": 1}, 200)
		waitFor(t, func() bool { return s.Feeds["BTC-USD"].Ticker().Last == int64(price) })
	}
	trade(10_400)
	post(5001, map[string]any{"side": "sell", "price": 11_400, "qty": 2}, 200)
	if _, e := post(5001, map[string]any{"side": "sell", "price": 11_500, "qty": 2}, 422); e.Error != "price_outside_band" {
		t.Fatalf("ask above the collar: %+v", e)
	}
	trade(9_600)
	mkt, _ := post(5002, map[string]any{"side": "buy", "type": "market", "qty": 2}, 200)
	if mkt.Filled != 0 || mkt.Status != "cancelled" {
		t.Fatalf("market buy reached past the band: %+v", mkt)
	}
	var syms []symbolInfo
	_, raw := s.do(t, "GET", "/v1/symbols", 0, nil)
	json.Unmarshal(raw, &syms)
	if syms[0].BandLow != 8640 || syms[0].BandHigh != 10560 {
		t.Fatalf("band %+v", syms[0].band)
	}

	rest, _ := post(5003, map[string]any{"side": "buy", "price": 9_500, "qty": 1}, 200)
	e, _ := s.Exchange.Engine("BTC-USD")
	var ttl int64
	e.Query(context.Background(), func(b *book.Book) {
		if o := b.Order(rest.OrderID); o != nil {
			ttl = o.ExpireAt
		}
	})
	if ttl == 0 {
		t.Fatal("resting order from outside has no expiry")
	}
	code, raw := s.do(t, "PATCH", fmt.Sprintf("/v1/orders/%d", rest.OrderID), 5003, map[string]any{"price": 5_000})
	if code != 422 || !strings.Contains(string(raw), "price_outside_band") {
		t.Fatalf("amend out of band: %d %s", code, raw)
	}
	code, raw = s.do(t, "PATCH", fmt.Sprintf("/v1/orders/%d", rest.OrderID), 5003, map[string]any{"qty": 1000})
	if code != 422 || !strings.Contains(string(raw), "qty_above_max") {
		t.Fatalf("amend above max qty: %d %s", code, raw)
	}
}

func TestClientOrderIDMakesRetriesSafe(t *testing.T) {
	s := startStack(t, Config{}, 0, nil)
	body := map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 100, "qty": 3, "client_order_id": "abc-1"}
	first := s.order(t, "POST", "/v1/orders", 7001, body, 200)
	again := s.order(t, "POST", "/v1/orders", 7001, body, 200)
	if first.OrderID == 0 || again.OrderID != first.OrderID || again.ClientOrderID != "abc-1" {
		t.Fatalf("first %+v, retry %+v", first, again)
	}
	other := s.order(t, "POST", "/v1/orders", 7002, body, 200)
	if other.OrderID == first.OrderID {
		t.Fatal("client order ids are not per trader")
	}
	code, raw := s.do(t, "GET", "/v1/orders?symbol=BTC-USD", 7001, nil)
	var open []openOrder
	json.Unmarshal(raw, &open)
	if code != 200 || len(open) != 1 {
		t.Fatalf("the retry placed a second order: %s", raw)
	}
	bad := map[string]any{"symbol": "BTC-USD", "side": "buy", "price": 100, "qty": 3, "client_order_id": "no spaces"}
	if code, _ := s.do(t, "POST", "/v1/orders", 7001, bad); code != 400 {
		t.Fatalf("invalid client order id: %d", code)
	}
}

func TestDedupeForgetsOldIDs(t *testing.T) {
	d := newDedupe(time.Minute, 2)
	now := time.Now()
	for i, id := range []string{"a", "b", "c"} {
		e, owner := d.claim(clientKey{1, id}, now.Add(time.Duration(i)*time.Second))
		if !owner {
			t.Fatalf("%s already claimed", id)
		}
		d.finish(e, response{status: 200})
	}
	if _, owner := d.claim(clientKey{1, "a"}, now.Add(3*time.Second)); !owner {
		t.Fatal("oldest id kept beyond the size bound")
	}
	if _, owner := d.claim(clientKey{1, "c"}, now.Add(4*time.Second)); owner {
		t.Fatal("recent id forgotten")
	}
	if _, owner := d.claim(clientKey{1, "c"}, now.Add(2*time.Minute)); !owner {
		t.Fatal("id kept past the window")
	}
}

func TestClientAddressUsesTrustedHops(t *testing.T) {
	s := &Server{cfg: Config{ProxyHops: 1}}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	r.Header.Add("X-Forwarded-For", "6.6.6.6, 1.2.3.4")
	if got := s.clientAddr(r); got != "1.2.3.4" {
		t.Fatalf("one trusted hop: %s, want the right-most entry", got)
	}
	s.cfg.ProxyHops = 0
	if got := s.clientAddr(r); got != "10.0.0.1" {
		t.Fatalf("no trusted hops: %s", got)
	}
}

func TestLimiterIsBounded(t *testing.T) {
	l := newLimiter(1, 2, 2)
	now := time.Now()
	if !l.allow("a", now) || !l.allow("b", now) {
		t.Fatal("first requests refused")
	}
	if l.allow("c", now) {
		t.Fatal("a third address was admitted to a full table")
	}
	if !l.allow("c", now.Add(3*time.Second)) {
		t.Fatal("idle buckets were not swept to make room")
	}
}

func TestBodyErrorsAreClean(t *testing.T) {
	s := startStack(t, Config{}, 0, nil)
	code, raw := s.do(t, "POST", "/v1/orders", 5000, map[string]any{"symbol": "BTC-USD", "side": "buy", "price": "high", "qty": 1})
	if code != 400 || strings.Contains(string(raw), "Go ") || strings.Contains(string(raw), "int64") {
		t.Fatalf("type error: %d %s", code, raw)
	}
	big := map[string]any{"symbol": strings.Repeat("x", 5000)}
	if code, _ := s.do(t, "POST", "/v1/orders", 5000, big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", code)
	}
	if code, raw := s.do(t, "GET", "/healthz", 0, nil); code != 200 || string(raw) != "ok\n" {
		t.Fatalf("healthz %d %q", code, raw)
	}
}
