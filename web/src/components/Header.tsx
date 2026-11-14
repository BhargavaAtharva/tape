import type { FeedStatus } from "../lib/feed";
import { formatChange, formatPrice } from "../lib/format";
import type { SymbolInfo } from "../lib/types";
import styles from "./Header.module.css";

interface Props {
  symbols: SymbolInfo[] | null;
  active: string;
  onSelect: (s: string) => void;
  status: FeedStatus;
  theme: "dark" | "light";
  onTheme: () => void;
}

export function Header({ symbols, active, onSelect, status, theme, onTheme }: Props) {
  return (
    <header className={styles.header}>
      <div className={`num ${styles.brand}`}>tape</div>
      <nav className={styles.strip} aria-label="Symbols">
        {symbols === null && <span className="muted">Loading symbols</span>}
        {symbols?.map((s) => {
          const up = s.last >= s.open;
          return (
            <button
              key={s.symbol}
              type="button"
              className={`${styles.sym} ${s.symbol === active ? styles.active : ""}`}
              aria-current={s.symbol === active ? "true" : undefined}
              onClick={() => onSelect(s.symbol)}
            >
              <span className={styles.name}>{s.symbol}</span>
              <span className="num">{s.last ? formatPrice(s.last, s.price_scale) : "–"}</span>
              <span className={`num ${s.trades ? (up ? "bid" : "ask") : "muted"}`}>{formatChange(s.last, s.open)}</span>
            </button>
          );
        })}
      </nav>
      <div className={styles.right}>
        <span className={`${styles.status} ${styles[status]}`}>
          <i aria-hidden="true" />
          {status}
        </span>
        <button
          type="button"
          className={styles.theme}
          onClick={onTheme}
          aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
        >
          {theme === "dark" ? "Light" : "Dark"}
        </button>
      </div>
    </header>
  );
}
