import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { EngineStats } from "./components/EngineStats";
import { Header } from "./components/Header";
import { MyOrders } from "./components/MyOrders";
import { OrderBook } from "./components/OrderBook";
import { OrderEntry } from "./components/OrderEntry";
import { PriceChart } from "./components/PriceChart";
import { TradesTape } from "./components/TradesTape";
import { api, ApiError } from "./lib/api";
import { levelKey, useMarket } from "./lib/feed";
import { traderId } from "./lib/trader";
import type { OpenOrder, OrderResult, Side, Stats, SymbolInfo } from "./lib/types";
import styles from "./App.module.css";

const TABS = ["book", "chart", "trade", "tape", "engine"] as const;
type Tab = (typeof TABS)[number];

function usePoll(fn: () => void, ms: number) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    ref.current();
    const id = setInterval(() => ref.current(), ms);
    return () => clearInterval(id);
  }, [ms]);
}

function savedTheme(): "dark" | "light" {
  try {
    const t = localStorage.getItem("tape.theme");
    if (t === "dark" || t === "light") return t;
  } catch {
    // Storage can be unavailable; fall back to the system preference.
  }
  return matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

export function App() {
  const trader = useMemo(traderId, []);
  const [symbols, setSymbols] = useState<SymbolInfo[] | null>(null);
  const [active, setActive] = useState(() => decodeURIComponent(location.hash.slice(1)));
  const [stats, setStats] = useState<Stats | null>(null);
  const [statsError, setStatsError] = useState(false);
  const [rates, setRates] = useState<number[]>([]);
  const [orders, setOrders] = useState<OpenOrder[] | null>(null);
  const [ordersError, setOrdersError] = useState("");
  const [own, setOwn] = useState<Set<number>>(() => new Set());
  const [picked, setPicked] = useState<{ price: number; side: Side; at: number } | null>(null);
  const [tab, setTab] = useState<Tab>("book");
  const [theme, setTheme] = useState(savedTheme);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  usePoll(() => {
    api.symbols().then(setSymbols, () => setSymbols((s) => s ?? []));
  }, 2000);

  usePoll(() => {
    api.stats().then(
      (s) => {
        setStats(s);
        setStatsError(false);
        setRates((h) => [...h.slice(-59), s.orders_per_sec]);
      },
      () => setStatsError(true),
    );
  }, 1000);

  const symbol = active && symbols?.some((s) => s.symbol === active) ? active : (symbols?.[0]?.symbol ?? "");
  const info = symbols?.find((s) => s.symbol === symbol);
  const scale = info?.price_scale ?? 2;

  useEffect(() => {
    if (symbol) history.replaceState(null, "", `#${encodeURIComponent(symbol)}`);
  }, [symbol]);

  const market = useMarket(symbol);

  const refreshOrders = useCallback(() => {
    if (!symbol) return;
    api.openOrders(symbol, trader).then(
      (o) => {
        setOrders(o);
        setOrdersError("");
        setOwn((prev) => {
          if (o.every((x) => prev.has(x.order_id))) return prev;
          const next = new Set(prev);
          o.forEach((x) => next.add(x.order_id));
          return next;
        });
      },
      (err) => setOrdersError(err instanceof ApiError ? err.message : "Could not load orders"),
    );
  }, [symbol, trader]);

  useEffect(() => {
    setOrders(null);
    refreshOrders();
    const id = setInterval(refreshOrders, 5000);
    return () => clearInterval(id);
  }, [refreshOrders]);

  // A fill against one of my resting orders arrives only through the public
  // tape, so watch it and refresh the order list when one of mine trades.
  const seenSeq = useRef(0);
  useEffect(() => {
    const fresh = market.trades.filter((t) => t.seq > seenSeq.current);
    if (!fresh.length) return;
    seenSeq.current = fresh.at(-1)!.seq;
    if (fresh.some((t) => own.has(t.maker_order_id))) refreshOrders();
  }, [market.version, market.trades, own, refreshOrders]);

  useEffect(() => {
    seenSeq.current = 0;
  }, [symbol]);

  const onResult = useCallback(
    (r: OrderResult) => {
      if (r.order_id) setOwn((prev) => new Set(prev).add(r.order_id));
      refreshOrders();
    },
    [refreshOrders],
  );

  const cancel = useCallback(
    (id: number) => {
      api.cancel(trader, id).finally(refreshOrders);
    },
    [trader, refreshOrders],
  );

  const cancelAll = useCallback(() => {
    Promise.allSettled((orders ?? []).map((o) => api.cancel(trader, o.order_id))).finally(refreshOrders);
  }, [orders, trader, refreshOrders]);

  const mine = useMemo(() => new Set((orders ?? []).map((o) => levelKey(o.side, o.price))), [orders]);
  const bestBid = Math.max(0, ...market.bids.keys()) || undefined;
  const bestAsk = market.asks.size ? Math.min(...market.asks.keys()) : undefined;

  const onPick = useCallback((price: number, side: Side) => {
    setPicked({ price, side, at: Date.now() });
    setTab((t) => (t === "book" ? "trade" : t));
  }, []);

  const select = (s: string) => {
    setActive(s);
    setPicked(null);
  };

  if (symbols && symbols.length === 0) {
    return (
      <div className={styles.fatal}>
        <p className="num">tape</p>
        <p>Cannot reach the exchange. Retrying.</p>
      </div>
    );
  }

  return (
    <div className={styles.app}>
      <Header
        symbols={symbols}
        active={symbol}
        onSelect={select}
        status={market.status}
        theme={theme}
        onTheme={() => {
          const next = theme === "dark" ? "light" : "dark";
          setTheme(next);
          try {
            localStorage.setItem("tape.theme", next);
          } catch {
            // Theme still applies for this session.
          }
        }}
      />
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
          <OrderEntry
            symbol={symbol}
            scale={scale}
            trader={trader}
            picked={picked}
            bestBid={bestBid}
            bestAsk={bestAsk}
            onResult={onResult}
          />
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
