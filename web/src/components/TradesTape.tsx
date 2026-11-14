import { formatPrice, formatQty, formatTime } from "../lib/format";
import type { Trade } from "../lib/types";
import { Empty, Panel } from "./Panel";
import styles from "./TradesTape.module.css";

interface Props {
  trades: Trade[];
  scale: number;
  own: Set<number>;
}

const SHOWN = 80;

export function TradesTape({ trades, scale, own }: Props) {
  const recent = trades.slice(-SHOWN).reverse();
  return (
    <Panel title="Trades" extra={<span className="num">{trades.length ? `${trades.length} loaded` : ""}</span>} className={styles.panel}>
      {recent.length === 0 ? (
        <Empty>No trades yet</Empty>
      ) : (
        <div className={styles.list}>
          <div className={`${styles.row} ${styles.head}`}>
            <span>Time</span>
            <span>Price</span>
            <span>Size</span>
            <span>Aggressor</span>
          </div>
          {recent.map((t) => {
            const mine = own.has(t.maker_order_id) || own.has(t.taker_order_id);
            return (
              <div key={t.seq} className={`${styles.row} ${mine ? styles.mine : ""}`}>
                <span className="num muted">{formatTime(t.time)}</span>
                <span className={`num ${t.side === "buy" ? "bid" : "ask"}`}>{formatPrice(t.price, scale)}</span>
                <span className="num">{formatQty(t.qty)}</span>
                <span className={styles.tag}>
                  {t.side === "buy" ? "buyer" : "seller"}
                  {mine && <b>you</b>}
                </span>
              </div>
            );
          })}
        </div>
      )}
    </Panel>
  );
}
