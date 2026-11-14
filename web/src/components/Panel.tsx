import type { ReactNode } from "react";
import styles from "./Panel.module.css";

interface Props {
  title: string;
  extra?: ReactNode;
  className?: string;
  children: ReactNode;
}

export function Panel({ title, extra, className, children }: Props) {
  return (
    <section className={`${styles.panel} ${className ?? ""}`} aria-label={title}>
      <header className={styles.head}>
        <h2 className="label">{title}</h2>
        {extra && <div className={styles.extra}>{extra}</div>}
      </header>
      <div className={styles.body}>{children}</div>
    </section>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className={styles.empty}>{children}</div>;
}
