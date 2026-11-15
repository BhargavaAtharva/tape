import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { EngineStats } from "./components/EngineStats";
import { Header } from "./components/Header";
import { MyOrders } from "./components/MyOrders";
import { OrderBook } from "./components/OrderBook";
import { OrderEntry } from "./components/OrderEntry";
import { PriceChart } from "./components/PriceChart";
import { TradesTape } from "./components/TradesTape";
import { api } from "./lib/api";
import { levelKey, useMarket } from "./lib/feed";
import { useMyOrders } from "./lib/orders";
import { useTheme } from "./lib/theme";
import { traderId } from "./lib/trader";
import type { Side, Stats, SymbolInfo } from "./lib/types";
import styles from "./App.module.css";

const TABS = ["book", "chart", "trade", "tape", "engine"] as const;
type Tab = (typeof TABS)[number];

// Market data streams; these two only fill the header strip and the stats
// panel, so they are polled slowly, and not at all while the page is hidden.
const SYMBOLS_POLL_MS = 10_000;
const STATS_POLL_MS = 5_000;
// While the server is waking from sleep, try again this often.
const WAKE_RETRY_MS = 3_000;

function usePoll(fn: () => void, ms: number) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    let id = 0;
    const start = () => {
      ref.current();
      id = window.setInterval(() => ref.current(), ms);
    };
    const onVisibility = () => {
      clearInterval(id);
      if (!document.hidden) start();
    };
    if (!document.hidden) start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [ms]);
}

export function App() {
  const trader = useMemo(traderId, []);
  const [symbols, setSymbols] = useState<SymbolInfo[] | null>(null);
  const [waking, setWaking] = useState(false);
  const [active, setActive] = useState(() => decodeURIComponent(location.hash.slice(1)));
  const [stats, setStats] = useState<Stats | null>(null);
  const [statsError, setStatsError] = useState(false);
  const [rates, setRates] = useState<number[]>([]);
  const [picked, setPicked] = useState<{ price: number; side: Side; at: number } | null>(null);
  const [tab, setTab] = useState<Tab>("book");
  const [theme, toggleTheme] = useTheme();

  const loadSymbols = useCallback(() => {
    api.symbols().then(
      (s) => {
        setSymbols(s);
        setWaking(false);
      },
      () => setWaking(true),
    );
  }, []);
  usePoll(loadSymbols, SYMBOLS_POLL_MS);

  // A free-tier server sleeps when idle and takes about a minute to start;
  // until the first answer, keep knocking more often than the normal poll.
  useEffect(() => {
    if (symbols || !waking) return;
    const id = setInterval(loadSymbols, WAKE_RETRY_MS);
    return () => clearInterval(id);
  }, [symbols, waking, loadSymbols]);

  usePoll(() => {
    if (!symbols) return;
    api.stats().then(
      (s) => {
        setStats(s);
        setStatsError(false);
        setRates((h) => [...h.slice(-59), s.orders_per_sec]);
      },
      () => setStatsError(true),
    );
  }, STATS_POLL_MS);

  const symbol = active && symbols?.some((s) => s.symbol === active) ? active : (symbols?.[0]?.symbol ?? "");
  const info = symbols?.find((s) => s.symbol === symbol);
  const scale = info?.price_scale ?? 2;

  useEffect(() => {
    if (symbol) history.replaceState(null, "", `#${encodeURIComponent(symbol)}`);
  }, [symbol]);

  const market = useMarket(symbol);
  const { orders, error: ordersError, own } = useMyOrders(symbol, trader);

  const cancel = useCallback(
    (id: number) => {
      api.cancel(trader, id).catch(() => undefined);
    },
    [trader],
  );

  const cancelAll = useCallback(() => {
    for (const o of orders ?? []) api.cancel(trader, o.order_id).catch(() => undefined);
  }, [orders, trader]);

  const mine = useMemo(() => new Set((orders ?? []).map((o) => levelKey(o.side, o.price))), [orders]);
  const bestBid = market.bids.size ? Math.max(...market.bids.keys()) : undefined;
  const bestAsk = market.asks.size ? Math.min(...market.asks.keys()) : undefined;

  const onPick = useCallback((price: number, side: Side) => {
    setPicked({ price, side, at: Date.now() });
    setTab((t) => (t === "book" ? "trade" : t));
  }, []);

  const select = (s: string) => {
    setActive(s);
    setPicked(null);
  };

  if (!symbols) {
    return (
      <div className={styles.fatal} role="status" aria-live="polite">
        <p className="num">tape</p>
        {waking ? (
          <>
            <p>Waking the demo server, about a minute.</p>
            <p className="muted">It sleeps when nobody is trading. This page connects by itself once it is up.</p>
            <span className={styles.waking} aria-hidden="true" />
          </>
        ) : (
          <p className="muted">Connecting</p>
        )}
      </div>
    );
  }

  return (
    <div className={styles.app}>
      <Header symbols={symbols} active={symbol} onSelect={select} status={market.status} theme={theme} onTheme={toggleTheme} />
      <nav className={styles.tabs} aria-label="Sections">
        {TABS.map((t) => (
          <button key={t} type="button" className={t === tab ? styles.tabOn : undefined} aria-pressed={t === tab} onClick={() => setTab(t)}>
            {t}
          </button>
        ))}
      </nav>
      <main className={styles.grid} data-tab={tab}>
        <div className={styles.book}>
          <OrderBook view={market} scale={scale} mine={mine} depth={40} onPick={onPick} />
        </div>
        <div className={styles.chart}>
          <PriceChart trades={market.trades} version={market.version} scale={scale} />
        </div>
        <div className={styles.trade}>
          <OrderEntry symbol={symbol} info={info} trader={trader} picked={picked} bestBid={bestBid} bestAsk={bestAsk} />
          <MyOrders orders={orders} error={ordersError} scale={scale} trader={trader} onCancel={cancel} onCancelAll={cancelAll} />
        </div>
        <div className={styles.tape}>
          <TradesTape trades={market.trades} scale={scale} own={own} />
        </div>
        <div className={styles.engine}>
          <EngineStats stats={stats} history={rates} symbol={symbol} error={statsError} />
        </div>
      </main>
    </div>
  );
}
