import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError } from "../lib/api";
import { formatPrice, formatQty, parsePrice } from "../lib/format";
import type { OrderResult, Side, SymbolInfo } from "../lib/types";
import { Panel } from "./Panel";
import styles from "./OrderEntry.module.css";

type Kind = "limit" | "market";
type TIF = "gtc" | "ioc" | "fok";

interface Props {
  symbol: string;
  info?: SymbolInfo;
  trader: number;
  picked: { price: number; side: Side; at: number } | null;
  bestBid?: number;
  bestAsk?: number;
}

interface Outcome {
  tone: "ok" | "warn" | "error";
  text: string;
}

const REASONS: Record<string, string> = {
  post_only_would_cross: "Post-only order would cross the book",
  fok_unfillable: "Not enough liquidity to fill the whole order",
  ioc_remainder: "Unfilled remainder cancelled",
  no_liquidity: "Book ran out of liquidity",
  self_trade: "Stopped to avoid trading with your own order",
  invalid_price: "Invalid price",
  invalid_qty: "Invalid quantity",
  too_many_open_orders: "Too many open orders on this symbol",
  price_outside_band: "Price is outside the allowed band",
  qty_above_max: "Quantity is above the per-order limit",
  notional_above_max: "Order value is above the per-order limit",
  rate_limited: "Too many requests; slow down",
  busy: "Exchange is busy; try again",
};

function describe(r: OrderResult, scale: number, price: number | null): Outcome {
  const reason = r.reason ? (REASONS[r.reason] ?? r.reason.replace(/_/g, " ")) : "";
  switch (r.status) {
    case "rejected":
      return { tone: "error", text: `Rejected: ${reason}` };
    case "filled":
      return { tone: "ok", text: `Filled ${formatQty(r.filled)}` };
    case "resting":
      return {
        tone: "ok",
        text: `${r.filled ? `Filled ${formatQty(r.filled)}, ` : ""}${formatQty(r.remaining)} resting${price ? ` at ${formatPrice(price, scale)}` : ""}`,
      };
    case "cancelled":
      return { tone: r.filled ? "ok" : "warn", text: `${r.filled ? `Filled ${formatQty(r.filled)}. ` : ""}${reason}` };
  }
}

export function OrderEntry({ symbol, info, trader, picked, bestBid, bestAsk }: Props) {
  const scale = info?.price_scale ?? 2;
  const [side, setSide] = useState<Side>("buy");
  const [kind, setKind] = useState<Kind>("limit");
  const [tif, setTif] = useState<TIF>("gtc");
  const [postOnly, setPostOnly] = useState(false);
  const [price, setPrice] = useState("");
  const [qty, setQty] = useState("10");
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);

  useEffect(() => {
    setPrice("");
    setOutcome(null);
  }, [symbol]);

  useEffect(() => {
    if (!picked) return;
    setKind("limit");
    setPrice(formatPrice(picked.price, scale).replace(/,/g, ""));
    // Clicking an ask suggests buying it, and the other way round.
    setSide(picked.side === "sell" ? "buy" : "sell");
  }, [picked, scale]);

  useEffect(() => {
    if (kind === "market" && tif === "gtc") setTif("ioc");
    if (kind === "market" || tif !== "gtc") setPostOnly(false);
  }, [kind, tif]);

  const ticks = kind === "limit" ? parsePrice(price, scale) : null;
  const lots = /^\d+$/.test(qty.trim()) ? Number(qty) : NaN;
  const outside = ticks !== null && info?.band_low && (ticks < info.band_low || ticks > info.band_high);
  const priceError =
    kind === "limit" && price !== "" && ticks === null
      ? `Up to ${scale} decimals`
      : outside
        ? `Allowed ${formatPrice(info!.band_low, scale)} to ${formatPrice(info!.band_high, scale)}`
        : "";
  const qtyError =
    qty !== "" && !(lots > 0 && Number.isSafeInteger(lots))
      ? "Whole lots only"
      : info?.max_qty && lots > info.max_qty
        ? `At most ${formatQty(info.max_qty)} per order`
        : "";
  const ready = !busy && lots > 0 && !qtyError && !priceError && (kind === "market" || ticks !== null);
  const reference = kind === "limit" ? ticks : side === "buy" ? bestAsk : bestBid;
  const notional = reference && lots > 0 ? reference * lots : null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready) return;
    setBusy(true);
    setOutcome(null);
    // One id per click: if the response is lost, a resend of the same
    // request cannot place a second order.
    const clientOrderId = crypto.randomUUID();
    try {
      const r = await api.place(trader, {
        client_order_id: clientOrderId,
        symbol,
        side,
        type: kind,
        tif,
        post_only: postOnly || undefined,
        price: kind === "limit" ? ticks! : undefined,
        qty: lots,
      });
      setOutcome(describe(r, scale, ticks));
    } catch (err) {
      const text = err instanceof ApiError ? (REASONS[err.code] ?? err.message) : "Request failed";
      setOutcome({ tone: "error", text });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel title="Order entry" className={styles.panel}>
      <form className={styles.form} onSubmit={submit} noValidate>
        <div className={styles.seg} role="radiogroup" aria-label="Side">
          {(["buy", "sell"] as const).map((s) => (
            <button
              key={s}
              type="button"
              role="radio"
              aria-checked={side === s}
              className={side === s ? (s === "buy" ? styles.buyOn : styles.sellOn) : undefined}
              onClick={() => setSide(s)}
            >
              {s === "buy" ? "Buy" : "Sell"}
            </button>
          ))}
        </div>
        <div className={styles.seg} role="radiogroup" aria-label="Order type">
          {(["limit", "market"] as const).map((k) => (
            <button
              key={k}
              type="button"
              role="radio"
              aria-checked={kind === k}
              className={kind === k ? styles.on : undefined}
              onClick={() => setKind(k)}
            >
              {k === "limit" ? "Limit" : "Market"}
            </button>
          ))}
        </div>

        <label className={styles.field}>
          <span className="label">Price</span>
          <div className={styles.inputRow}>
            <input
              className="num"
              inputMode="decimal"
              autoComplete="off"
              placeholder={
                kind === "market" ? "Market" : bestBid ? formatPrice(side === "buy" ? bestBid : (bestAsk ?? bestBid), scale) : "0.00"
              }
              disabled={kind === "market"}
              value={kind === "market" ? "" : price}
              onChange={(e) => setPrice(e.target.value)}
              aria-invalid={!!priceError}
            />
            <button
              type="button"
              className={styles.quick}
              disabled={kind === "market" || !bestBid}
              onClick={() => bestBid && setPrice(formatPrice(bestBid, scale).replace(/,/g, ""))}
            >
              Bid
            </button>
            <button
              type="button"
              className={styles.quick}
              disabled={kind === "market" || !bestAsk}
              onClick={() => bestAsk && setPrice(formatPrice(bestAsk, scale).replace(/,/g, ""))}
            >
              Ask
            </button>
          </div>
          {priceError && <span className={styles.hint}>{priceError}</span>}
        </label>

        <label className={styles.field}>
          <span className="label">Quantity</span>
          <input
            className="num"
            inputMode="numeric"
            autoComplete="off"
            value={qty}
            onChange={(e) => setQty(e.target.value)}
            aria-invalid={!!qtyError}
          />
          {qtyError && <span className={styles.hint}>{qtyError}</span>}
        </label>

        <div className={styles.row2}>
          <label className={styles.field}>
            <span className="label">Time in force</span>
            <select value={tif} onChange={(e) => setTif(e.target.value as TIF)}>
              {kind === "limit" && <option value="gtc">GTC</option>}
              <option value="ioc">IOC</option>
              <option value="fok">FOK</option>
            </select>
          </label>
          <label className={styles.check}>
            <input
              type="checkbox"
              checked={postOnly}
              disabled={kind !== "limit" || tif !== "gtc"}
              onChange={(e) => setPostOnly(e.target.checked)}
            />
            <span>Post only</span>
          </label>
        </div>

        <div className={styles.summary}>
          <span className="muted">Notional</span>
          <span className="num">{notional ? formatPrice(notional, scale) : "–"}</span>
        </div>

        <button type="submit" className={`${styles.submit} ${side === "buy" ? styles.submitBuy : styles.submitSell}`} disabled={!ready}>
          {busy ? "Sending" : `${side === "buy" ? "Buy" : "Sell"} ${symbol}`}
        </button>
        <p className={`${styles.outcome} ${outcome ? styles[outcome.tone] : ""}`} role="status" aria-live="polite">
          {outcome?.text ?? ""}
        </p>
      </form>
    </Panel>
  );
}
