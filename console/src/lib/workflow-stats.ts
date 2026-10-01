// What a workflow's statistics page draws, worked out from what the statistics routes answer: the
// figures along its top, the share of runs that did not succeed, a step's exit codes, its heatmap,
// and the share of items its steps refused. Each is a function of the answers alone, so that what
// the page shows is what the routes said.

import type { components } from "../api/schema";
import { took } from "./format";

export type RunsSeries = components["schemas"]["statsRuns"];
export type RunsBucket = components["schemas"]["statsRunsBucket"];
export type StepsSeries = components["schemas"]["statsSteps"];
export type StepSeries = StepsSeries["steps"][number];
export type StepBucket = components["schemas"]["statsStepBucket"];
export type PortsSeries = components["schemas"]["statsPorts"];

type Counts = RunsBucket["runs"];

// ended is the runs of a bucket that have ended, which a share of those that did not succeed is
// taken over: "the share that did not succeed is failed, cancelled and timed_out over the runs that
// ended".
export function ended(c: Counts): number {
  return c.succeeded + c.failed + c.cancelled + c.timed_out;
}

export function created(c: Counts): number {
  return ended(c) + c.queued + c.running + c.waiting;
}

// A figure along the top of the page: its value, its change since the span before where the page
// compares, and a line under it saying what it is made of.
export type Figure = { label: string; value: string; change?: string; aside: string };

const minus = "−";

function signed(n: number, written: (n: number) => string): string {
  if (n === 0) return "±0";
  return `${n > 0 ? "+" : minus}${written(Math.abs(n))}`;
}

// percent is the change of a count, as a share of what it was; none where it was nothing, since a
// change from nothing is no percentage.
function percent(now: number, before: number): string | undefined {
  if (before === 0) return undefined;
  return signed(Math.round(((now - before) / before) * 100), (n) => `${n}%`);
}

const share = (n: number, of: number) => (of === 0 ? undefined : (n / of) * 100);

export const count = (n: number) => Math.round(n).toLocaleString("en-GB");

// figures are the five figures of the page: the runs created, the share of those that ended that
// succeeded, the p95 of their duration and of their tasks' queue wait, and the step chosen's
// throughput where it fans out, or else the p95 of its duration. Each is read from the range taken
// as one bucket, since percentiles of buckets do not combine into the range's, and its change from
// the span before taken the same way.
export function figures(runs: RunsSeries, step: StepSeries | undefined, compare: boolean): Figure[] {
  const now = runs.overall;
  const before = compare ? runs.previous?.overall : undefined;
  const out: Figure[] = [];

  const n = created(now.runs);
  out.push({
    label: "Runs",
    value: count(n),
    change: before ? percent(n, created(before.runs)) : undefined,
    aside: before ? `${count(created(before.runs))} in the span before` : "created over the range",
  });

  const done = ended(now.runs);
  const ok = share(now.runs.succeeded, done);
  const okBefore = before ? share(before.runs.succeeded, ended(before.runs)) : undefined;
  const others = (
    [
      ["failed", now.runs.failed],
      ["timed out", now.runs.timed_out],
      ["cancelled", now.runs.cancelled],
    ] as const
  )
    .filter(([, v]) => v > 0)
    .map(([k, v]) => `${count(v)} ${k}`);
  out.push({
    label: "Succeeded",
    value: ok === undefined ? "none ended" : `${ok.toFixed(1)}%`,
    change: ok !== undefined && okBefore !== undefined ? signed(Math.round((ok - okBefore) * 10) / 10, (v) => `${v.toFixed(1)} pts`) : undefined,
    aside: done === 0 ? "no run ended over the range" : `${count(now.runs.succeeded)} of ${count(done)} that ended${others.length ? `; ${others.join(", ")}` : ""}`,
  });

  const lasted = (label: string, p: RunsBucket["duration_ms"], was: RunsBucket["duration_ms"] | undefined, aside: (p: NonNullable<RunsBucket["duration_ms"]>) => string): Figure => ({
    label,
    value: p?.p95 === undefined ? "none" : took(p.p95),
    change: p?.p95 !== undefined && was?.p95 !== undefined ? signed(p.p95 - was.p95, took) : undefined,
    aside: p?.p95 === undefined ? "nothing to take it over" : aside(p),
  });
  out.push(lasted("Duration, p95", now.duration_ms, before?.duration_ms, (p) => `p50 ${took(p.p50 ?? 0)} · p99 ${took(p.p99 ?? 0)}`));
  out.push(lasted("Queue wait, p95", now.queue_wait_ms, before?.queue_wait_ms, (p) => `p50 ${took(p.p50 ?? 0)}, from ready to dispatched`));

  if (step?.overall) {
    const o = step.overall;
    const was = compare ? step.previous_overall : undefined;
    if (o.items_per_minute !== undefined) {
      out.push({
        label: `${step.step} throughput`,
        value: `${count(o.items_per_minute)} items/min`,
        change: was?.items_per_minute ? percent(o.items_per_minute, was.items_per_minute) : undefined,
        aside: "while its shards run",
      });
    } else {
      out.push(lasted(`${step.step}, p95`, o.duration_ms, was?.duration_ms, (p) => `p50 ${took(p.p50 ?? 0)}, ${count(o.attempts)} attempts`));
    }
  }
  return out;
}

// notSucceeded is, bucket by bucket, the share of the runs that ended which failed, timed out or
// were cancelled, each its own series, and none where no run of the bucket ended.
export function notSucceeded(buckets: RunsBucket[]): { failed: (number | null)[]; timed_out: (number | null)[]; cancelled: (number | null)[]; all: (number | null)[] } {
  const of = (pick: (c: Counts) => number) => buckets.map((b) => share(pick(b.runs), ended(b.runs)) ?? null);
  return {
    failed: of((c) => c.failed),
    timed_out: of((c) => c.timed_out),
    cancelled: of((c) => c.cancelled),
    all: of((c) => c.failed + c.timed_out + c.cancelled),
  };
}

// exitCodes are a step's attempts that ended other than with 0 over the range, by exit code, the
// most frequent first, an attempt that ended with none under null.
export function exitCodes(buckets: StepBucket[]): { code: number | null; attempts: number }[] {
  const by = new Map<number | null, number>();
  for (const b of buckets) {
    for (const e of b.exit_codes) {
      if (e.exit_code === 0) continue;
      by.set(e.exit_code, (by.get(e.exit_code) ?? 0) + e.attempts);
    }
  }
  return [...by.entries()].map(([code, attempts]) => ({ code, attempts })).sort((a, b) => b.attempts - a.attempts || (a.code ?? 256) - (b.code ?? 256));
}

// chosen is the step the page is about where the address names none: the one whose attempts failed
// most over the range, where any did, since that is the one a reader comes to look at, and the first
// the workflow runs otherwise.
export function chosen(steps: StepSeries[], named: string | null): StepSeries | undefined {
  const found = named ? steps.find((s) => s.step === named) : undefined;
  if (found) return found;
  let most: StepSeries | undefined;
  let failed = 0;
  for (const s of steps) {
    const n = exitCodes(s.buckets ?? []).reduce((a, e) => a + e.attempts, 0);
    if (n > failed) {
      most = s;
      failed = n;
    }
  }
  return most ?? steps[0];
}

// A heatmap's scale: five strengths of the accent from the shortest median to the longest, each
// with the least it holds, and the cells with nothing in them apart.
export type Heat = { levels: number[]; level: (ms: number | undefined) => number };

export function heat(medians: (number | undefined)[]): Heat {
  const known = medians.filter((m): m is number => m !== undefined);
  if (known.length === 0) return { levels: [], level: () => 0 };
  const low = Math.min(...known);
  const high = Math.max(...known);
  const width = (high - low) / 5;
  const levels = width === 0 ? [low] : [0, 1, 2, 3, 4].map((i) => low + i * width);
  return {
    levels,
    level: (ms) => {
      if (ms === undefined) return 0;
      if (width === 0) return 5;
      return Math.min(5, Math.floor((ms - low) / width) + 1);
    },
  };
}

// refused is, for each step that has a rejected or an error port, the share of the items it
// published that went there, bucket by bucket, and what the range came to: "the share rejected, or
// sent to error, is what the rejected or the error port published over what every output port of
// the step did". A step that published nothing there over the range is left out, as a line along
// zero says nothing.
export function refused(ports: PortsSeries): { lines: { step: string; port: "rejected" | "error"; values: (number | null)[] }[]; rejected: number; error: number; of: number } {
  const lines: { step: string; port: "rejected" | "error"; values: (number | null)[] }[] = [];
  let rejected = 0;
  let error = 0;
  let of = 0;
  for (const s of ports.steps) {
    const total = (items: Record<string, number>) => Object.values(items).reduce((a, n) => a + n, 0);
    let counted = false;
    for (const port of ["rejected", "error"] as const) {
      const sent = s.buckets.reduce((a, b) => a + ((b.items as Record<string, number>)[port] ?? 0), 0);
      if (sent === 0) continue;
      counted = true;
      if (port === "rejected") rejected += sent;
      else error += sent;
      lines.push({
        step: s.step,
        port,
        values: s.buckets.map((b) => {
          const items = b.items as Record<string, number>;
          return share(items[port] ?? 0, total(items)) ?? null;
        }),
      });
    }
    if (counted) of += s.buckets.reduce((a, b) => a + total(b.items as Record<string, number>), 0);
  }
  return { lines, rejected, error, of };
}
