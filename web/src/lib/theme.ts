import { useEffect, useState } from "react";

type Theme = "dark" | "light";
const KEY = "tape.theme";

function chosen(): Theme | null {
  try {
    const t = localStorage.getItem(KEY);
    return t === "dark" || t === "light" ? t : null;
  } catch {
    return null;
  }
}

const system = (): Theme => (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");

/**
 * useTheme follows the system colour scheme until the user picks one with
 * the toggle; the pick is remembered. The effective theme is always written
 * to <html data-theme>, which is the only thing the CSS looks at.
 */
export function useTheme(): [Theme, () => void] {
  const [pick, setPick] = useState(chosen);
  const [sys, setSys] = useState(system);

  useEffect(() => {
    const mq = matchMedia("(prefers-color-scheme: light)");
    const on = () => setSys(system());
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);

  const theme = pick ?? sys;
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  const toggle = () => {
    const next = theme === "dark" ? "light" : "dark";
    setPick(next);
    try {
      localStorage.setItem(KEY, next);
    } catch {
      // The choice still applies for this visit.
    }
  };
  return [theme, toggle];
}
