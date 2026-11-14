import { formatPrice, formatQty } from "../lib/format";
import type { OpenOrder } from "../lib/types";
import { Empty, Panel } from "./Panel";
import styles from "./MyOrders.module.css";

interface Props {
  orders: OpenOrder[] | null;
  error: string;
  scale: number;
  trader: number;
  onCancel: (id: number) => void;
  onCancelAll: () => void;
}

export function MyOrders({ orders, error, scale, trader, onCancel, onCancelAll }: Props) {
  const extra = (
    <>
      <span className="num">trader {trader}</span>
      {orders && orders.length > 1 && (
        <button type="button" className={styles.link} onClick={onCancelAll}>
          Cancel all
        </button>
      )}
    </>
  );
  let body;
  if (error) body = <Empty>{error}</Empty>;
  else if (!orders) body = <Empty>Loading</Empty>;
  else if (!orders.length) body = <Empty>No open orders</Empty>;
  else
    body = (
      <div className={styles.list}>
        <div className={`${styles.row} ${styles.head}`}>
          <span>Side</span>
          <span>Price</span>
          <span>Open</span>
          <span />
        </div>
        {orders.map((o) => (
          <div key={o.order_id} className={styles.row}>
            <span className={o.side === "buy" ? "bid" : "ask"}>{o.side === "buy" ? "Buy" : "Sell"}</span>
            <span className="num">{formatPrice(o.price, scale)}</span>
            <span className="num">{formatQty(o.qty)}</span>
            <button type="button" className={styles.cancel} onClick={() => onCancel(o.order_id)} aria-label={`Cancel order ${o.order_id}`}>
              Cancel
            </button>
          </div>
        ))}
      </div>
    );
  return (
    <Panel title="My orders" extra={extra} className={styles.panel}>
      {body}
    </Panel>
  );
}
