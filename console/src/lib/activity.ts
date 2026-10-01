// The home's year of activity: a square a day and a column a week, every namespace the caller reads
// added together, as the documentation's Web console sets it out.
//
// A square is a day in UTC and the weeks begin on Monday, as ISO 8601 has them: the squares are the
// buckets of GET /api/v1/{ns}/stats/runs asked for by the day, which fall on midnight in UTC, so that
// a square counts what every other chart counts that day and opens exactly the runs its bucket opens.

import type { components } from "../api/schema";

export type RunsSeries = components["schemas"]["statsRuns"];
export type Counts = components["schemas"]["statsRunsBucket"]["runs"];

const day = 86_400_000;

// weeks is how many columns the year is drawn in: 52 full weeks before the current one, and it.
export const weeks = 53;

// mondayOf is midnight in UTC of the Monday of the week an instant is in.
export function mondayOf(at: number): number {
  const midnight = Math.floor(at / day) * day;
  // getUTCDay counts Sunday as 0; a week here begins on Monday.
  const weekday = (new Date(midnight).getUTCDay() + 6) % 7;
  return midnight - weekday * day;
}

// yearOf is the range the squares cover: from the Monday 52 weeks before the current week's, to now.
export function yearOf(now: number): { from: Date; to: Date } {
  return { from: new Date(mondayOf(now) - (weeks - 1) * 7 * day), to: new Date(now) };
}

// dayOf is the day an instant is in, as the address and a square's key write it.
export function dayOf(at: number): string {
  return new Date(at).toISOString().slice(0, 10);
}

export const states = ["queued", "running", "waiting", "succeeded", "failed", "cancelled", "timed_out"] as const;

export function none(): Counts {
  return { queued: 0, running: 0, waiting: 0, succeeded: 0, failed: 0, cancelled: 0, timed_out: 0 };
}

export function total(c: Counts): number {
  return states.reduce((n, s) => n + c[s], 0);
}

// failures are the runs of a day that ended failed or timed out: what went wrong without anyone
// asking, a cancelled run being one somebody stopped.
export function failures(c: Counts): number {
  return c.failed + c.timed_out;
}

// added is the runs of every namespace's series by the day each bucket opens, the days of the series
// added together.
export function added(series: readonly RunsSeries[]): Map<string, Counts> {
  const out = new Map<string, Counts>();
  for (const s of series) {
    for (const b of s.buckets) {
      const key = b.since.slice(0, 10);
      const into = out.get(key) ?? none();
      for (const st of states) into[st] += b.runs[st];
      out.set(key, into);
    }
  }
  return out;
}

export type Square = { day: string; at: number; counts: Counts; future: boolean };

// grid is the squares, a column a week from the year's first Monday, Monday at the top; the days of
// the current week after today are drawn empty and say they are to come.
export function grid(now: number, counts: ReadonlyMap<string, Counts>): Square[][] {
  const first = mondayOf(now) - (weeks - 1) * 7 * day;
  const today = Math.floor(now / day) * day;
  const out: Square[][] = [];
  for (let w = 0; w < weeks; w++) {
    const column: Square[] = [];
    for (let d = 0; d < 7; d++) {
      const at = first + (w * 7 + d) * day;
      const key = dayOf(at);
      column.push({ day: key, at, counts: counts.get(key) ?? none(), future: at > today });
    }
    out.push(column);
  }
  return out;
}

// Shades: none, and four that split the days that counted something into quarters by their count, so
// that a year of a few runs a day and one of thousands each use every shade, and one busy day does not
// leave every other square looking empty, which a scale up to the busiest day would.
export type Shades = { bounds: number[]; level: (n: number) => number };

export function shades(values: readonly number[]): Shades {
  const counted = values.filter((v) => v > 0).sort((a, b) => a - b);
  if (counted.length === 0) return { bounds: [], level: () => 0 };
  const at = (q: number) => counted[Math.min(counted.length - 1, Math.floor(q * counted.length))]!;
  // The least count each shade from the second up starts at, never below the one before it.
  const bounds = [counted[0]!, at(0.25), at(0.5), at(0.75)];
  for (let i = 1; i < bounds.length; i++) bounds[i] = Math.max(bounds[i]!, bounds[i - 1]!);
  return {
    bounds,
    level: (n) => {
      if (n <= 0) return 0;
      let l = 1;
      for (let i = 1; i < bounds.length; i++) if (n >= bounds[i]! && bounds[i]! > bounds[i - 1]!) l = i + 1;
      return l;
    },
  };
}

// months are where each month's name sits above the columns: over the first column whose Monday is
// in it. A name squeezed against the one before is left out rather than written over it, save the
// year's first, a month the grid barely holds, which gives way to the next.
export function months(columns: readonly Square[][]): { column: number; name: string }[] {
  const out: { column: number; name: string }[] = [];
  let last = "";
  columns.forEach((c, i) => {
    const month = c[0]!.day.slice(0, 7);
    if (month === last) return;
    last = month;
    const label = { column: i, name: new Date(c[0]!.at).toLocaleString("en", { month: "short", timeZone: "UTC" }) };
    const before = out[out.length - 1];
    if (before && i - before.column < 3) {
      if (out.length === 1 && before.column === 0) out[0] = label;
      return;
    }
    out.push(label);
  });
  return out;
}

// bounds are the since and until of a day's bucket, both included, as GET /api/v1/runs takes them:
// the instants a statistics bucket carries for the runs it counts.
export function boundsOf(dayKey: string): { since: string; until: string } {
  return { since: `${dayKey}T00:00:00Z`, until: `${dayKey}T23:59:59.999999999Z` };
}

// lastWeek is a namespace's runs and failures over the seven days ending today, from its series.
export function lastWeek(s: RunsSeries, now: number): Week {
  const from = Math.floor(now / day) * day - 6 * day;
  const out = { runs: 0, failures: 0, succeeded: 0, ended: 0 };
  for (const b of s.buckets) {
    if (Date.parse(b.since) < from) continue;
    out.runs += total(b.runs);
    out.failures += failures(b.runs);
    out.succeeded += b.runs.succeeded;
    out.ended += b.runs.succeeded + b.runs.failed + b.runs.cancelled + b.runs.timed_out;
  }
  return out;
}

// Week is what a namespace did over the last seven days: its runs, those that went wrong, and of
// those that ended, how many succeeded, which the home's figures add up across namespaces.
export type Week = { runs: number; failures: number; succeeded: number; ended: number };

// together is the weeks of several namespaces added up.
export function together(weeks: readonly Week[]): Week {
  return weeks.reduce((a, w) => ({ runs: a.runs + w.runs, failures: a.failures + w.failures, succeeded: a.succeeded + w.succeeded, ended: a.ended + w.ended }), { runs: 0, failures: 0, succeeded: 0, ended: 0 });
}

// said is a day's count in words, for its square's title.
export function said(square: Square, by: "runs" | "failures"): string {
  const date = new Date(square.at).toLocaleDateString("en-GB", { weekday: "long", day: "numeric", month: "long", year: "numeric", timeZone: "UTC" });
  if (square.future) return `${date}: to come`;
  const n = by === "runs" ? total(square.counts) : failures(square.counts);
  const word = by === "runs" ? (n === 1 ? "run" : "runs") : n === 1 ? "failure" : "failures";
  const parts = states.filter((s) => square.counts[s] > 0).map((s) => `${square.counts[s]} ${s}`);
  return `${n === 0 ? "No" : n} ${word} on ${date}${parts.length && by === "runs" ? `: ${parts.join(", ")}` : ""}`;
}
