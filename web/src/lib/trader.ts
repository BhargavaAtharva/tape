const KEY = "tape.trader";

// Ids below 1000 belong to the server's bots; stay well clear of them.
function fresh(): number {
  return 100_000 + Math.floor(Math.random() * 900_000);
}

export function traderId(): number {
  try {
    const saved = Number(localStorage.getItem(KEY));
    if (Number.isSafeInteger(saved) && saved >= 1000) return saved;
    const id = fresh();
    localStorage.setItem(KEY, String(id));
    return id;
  } catch {
    return fresh();
  }
}
