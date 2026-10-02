import type { Route } from "./route";

// The palette, which : and Search at the top of the sidebar open over the screen: every view of the
// sidebar, the namespaces, the workflows and the latest runs the caller reads, and the keys of the
// screen drawn, narrowed as letters are typed. It matches as agk console's filter does
// (cmd/agk/internal/console/filter.go), so that one habit serves both.

// An Entry is one thing the palette opens or does: a place, reached by its route, or a key of the
// screen, done as the key would. id stays the same from one opening to the next, which is how the
// ones chosen last are found again.
export type Entry = {
  id: string;
  kind: "view" | "namespace" | "workflow" | "run" | "key";
  label: string;
  detail?: string;
  icon?: string;
  key?: string;
  state?: string;
  to?: Route;
  does?: () => void;
};

// A Found entry is one the words typed left, with the letters of its label they matched.
export type Found = { entry: Entry; score: number; at: number[] };

// matched is how closely a word matches a text, -1 where it does not: the word found whole scores
// highest, more where it starts a word of the text; otherwise its letters in order, a letter
// following the one before scoring more than one further on. at is where the matched letters are.
export function matched(word: string, text: string): { score: number; at: number[] } {
  const w = [...word.toLowerCase()];
  const t = [...text.toLowerCase()];
  const whole = t.join("").indexOf(w.join(""));
  if (whole >= 0) {
    const start = [...t.join("").slice(0, whole)].length;
    let score = 100 + w.length;
    if (start === 0 || !/[\p{L}\p{N}]/u.test(t[start - 1]!)) score += 50;
    return { score, at: w.map((_, i) => start + i) };
  }
  let score = 0;
  let j = 0;
  let last = -2;
  const at: number[] = [];
  for (let i = 0; i < t.length && j < w.length; i++) {
    if (t[i] !== w[j]) continue;
    score += i === last + 1 ? 5 : 1;
    last = i;
    at.push(i);
    j++;
  }
  return j < w.length ? { score: -1, at: [] } : { score, at };
}

// narrowed is the entries every word typed matches, the closest first and in their order among
// equals. A word matches an entry's label, or else its detail at half the score, so that a workflow
// is found by its namespace as well; a run is found by the start of its identifier, which is how one
// is read off a log or a link, or by its workflow.
export function narrowed(entries: readonly Entry[], typed: string): Found[] {
  const words = typed.trim().split(/\s+/).filter((w) => w !== "");
  if (words.length === 0) return entries.map((entry) => ({ entry, score: 0, at: [] }));
  const found: Found[] = [];
  for (const entry of entries) {
    let score = 0;
    const at = new Set<number>();
    let all = true;
    for (const word of words) {
      if (entry.kind === "run" && entry.label.toLowerCase().startsWith(word.toLowerCase())) {
        score += 200 + word.length;
        for (let i = 0; i < word.length; i++) at.add(i);
        continue;
      }
      const onLabel = entry.kind === "run" ? { score: -1, at: [] } : matched(word, entry.label);
      const onDetail = entry.detail ? matched(word, entry.detail) : { score: -1, at: [] };
      if (onLabel.score >= 0 && onLabel.score >= onDetail.score / 2) {
        score += onLabel.score;
        onLabel.at.forEach((i) => at.add(i));
      } else if (onDetail.score >= 0) {
        score += onDetail.score / 2;
      } else {
        all = false;
        break;
      }
    }
    if (all) found.push({ entry, score, at: [...at].sort((a, b) => a - b) });
  }
  return found.map((f, i) => ({ f, i })).sort((a, b) => b.f.score - a.f.score || a.i - b.i).map(({ f }) => f);
}

// A run's whole identifier, as the API writes one: 26 letters and digits of Crockford's base 32.
export const wholeRun = /^[0-9A-HJKMNP-TV-Z]{26}$/i;

// The entries chosen last, kept in the browser that chose them, five at most and the latest first.
const kept = "agentiik.palette.chosen";
const most = 5;

export function chosenLast(storage: Storage | undefined): string[] {
  try {
    const list: unknown = JSON.parse(storage?.getItem(kept) ?? "[]");
    return Array.isArray(list) ? list.filter((id): id is string => typeof id === "string").slice(0, most) : [];
  } catch {
    return [];
  }
}

export function choose(storage: Storage | undefined, id: string) {
  try {
    storage?.setItem(kept, JSON.stringify([id, ...chosenLast(storage).filter((other) => other !== id)].slice(0, most)));
  } catch {
    // A browser that keeps nothing opens the palette on everything rather than on what it chose.
  }
}

// opening is what the palette lists before anything is typed: the entries chosen last that are
// still listed, then every other.
export function opening(entries: readonly Entry[], last: readonly string[]): { recent: Entry[]; rest: Entry[] } {
  const byId = new Map(entries.map((e) => [e.id, e]));
  const recent = last.map((id) => byId.get(id)).filter((e): e is Entry => e !== undefined);
  const taken = new Set(recent.map((e) => e.id));
  return { recent, rest: entries.filter((e) => !taken.has(e.id)) };
}

// filtered is the rows of a list every word typed matches, the closest first and in their order
// among equals, a row matched by the text it is read by: what the Filter field at the head of a
// screen leaves of a list the API answered whole.
export function filtered<T>(rows: readonly T[], typed: string, text: (row: T) => string): T[] {
  const words = typed.trim().split(/\s+/).filter((w) => w !== "");
  if (words.length === 0) return [...rows];
  const kept: { row: T; score: number; i: number }[] = [];
  rows.forEach((row, i) => {
    let score = 0;
    for (const word of words) {
      const m = matched(word, text(row));
      if (m.score < 0) return;
      score += m.score;
    }
    kept.push({ row, score, i });
  });
  return kept.sort((a, b) => b.score - a.score || a.i - b.i).map((k) => k.row);
}
