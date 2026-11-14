const apiBase = (import.meta.env.VITE_API_URL ?? "").replace(/\/$/, "");

function wsBase(): string {
  const explicit = import.meta.env.VITE_WS_URL;
  if (explicit) return explicit.replace(/\/$/, "");
  if (apiBase) return apiBase.replace(/^http/, "ws");
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}`;
}

export const API_URL = apiBase;
export const WS_URL = wsBase();
