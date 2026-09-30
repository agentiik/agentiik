// The ground the console is drawn on: the reader's system's, or the one they chose, which the tokens
// read from data-theme on the root. The choice is kept in this browser alone, a convenience of one
// reader's, and a browser that keeps nothing follows the system.

export type Ground = "system" | "light" | "dark";

const key = "agentiik.ground";

export function chosen(storage: Storage | undefined): Ground {
  try {
    const kept = storage?.getItem(key);
    return kept === "light" || kept === "dark" ? kept : "system";
  } catch {
    return "system";
  }
}

export function apply(root: HTMLElement, storage: Storage | undefined, ground: Ground): void {
  if (ground === "system") {
    delete root.dataset.theme;
  } else {
    root.dataset.theme = ground;
  }
  try {
    if (ground === "system") {
      storage?.removeItem(key);
    } else {
      storage?.setItem(key, ground);
    }
  } catch {
    // A browser that keeps nothing still draws the ground asked for, until the page is loaded again.
  }
}
