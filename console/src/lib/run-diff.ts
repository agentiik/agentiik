// Two runs of one commit set side by side, to read a failed run against a good one: where they
// part, step by step, from what GET /api/v1/runs/{id} answers of each, and under run:read_data what
// a step's ports held.

import type { Envelope, Item } from "./envelope";
import { lastAttempt, type RunDetail, type StepSummary, type TaskSummary } from "./run.svelte";

// A step as the two runs ran it, and what tells them apart: the words name what differs, in the
// order a reader asks about them.
export type StepDiff = { step: string; a?: StepSummary; b?: StepSummary; differs: string[] };

// exits are the exit codes a step's last attempt of each shard ended with, sorted, none for a task
// that recorded none: two runs whose shards failed alike read alike whatever their order.
export function exits(run: RunDetail, step: string): string[] {
  return lastAttempt(run, step)
    .map((t) => (t.exit_code === undefined || t.exit_code === null ? "none" : String(t.exit_code)))
    .sort();
}

// summed is exit codes as a person reads a column of them: each code once, with how many shards
// ended with it where more than one did, the codes other than 0 first.
export function summed(codes: string[]): string {
  const by = new Map<string, number>();
  for (const c of codes) by.set(c, (by.get(c) ?? 0) + 1);
  return [...by.entries()]
    .sort(([a], [b]) => (a === "0" ? 1 : 0) - (b === "0" ? 1 : 0) || a.localeCompare(b))
    .map(([c, n]) => (n > 1 ? `${c} ×${n}` : c))
    .join(" · ");
}

// paramsOf is what the step's first shard was last dispatched with, which every shard of a step is
// given alike: its parameters, resolved, answered under run:read_data alone.
export function paramsOf(run: RunDetail, step: string): TaskSummary["params"] {
  return lastAttempt(run, step).find((t) => t.params !== undefined)?.params;
}

// canonical is a value written as JSON with every object's keys in order, so that two values equal
// as data are written alike whatever order their keys came in.
export function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v !== null && typeof v === "object") {
    const o = v as Record<string, unknown>;
    return `{${Object.keys(o)
      .sort()
      .filter((k) => o[k] !== undefined)
      .map((k) => `${JSON.stringify(k)}:${canonical(o[k])}`)
      .join(",")}}`;
  }
  return JSON.stringify(v);
}

const counts = (s?: StepSummary) => Object.fromEntries(Object.entries(s?.ports ?? {}).map(([p, e]) => [p, e.items]));

// diffSteps lays the two runs' steps side by side in the order the first ran them, then any step only
// the second has, and says of each what differs: its verdict, its attempts, the exit codes of its
// last attempts, the items on each port, its image, and under run:read_data its parameters. How long
// a step took is shown and never counted a difference, since no two runs take the same time.
export function diffSteps(a: RunDetail, b: RunDetail, readsData: boolean): StepDiff[] {
  const names = [...a.steps.map((s) => s.step), ...b.steps.map((s) => s.step).filter((n) => !a.steps.some((s) => s.step === n))];
  return names.map((step) => {
    const x = a.steps.find((s) => s.step === step);
    const y = b.steps.find((s) => s.step === step);
    const differs: string[] = [];
    if (!x || !y) {
      differs.push(x ? "only in the first run" : "only in the second run");
      return { step, a: x, b: y, differs };
    }
    if (x.verdict !== y.verdict) differs.push("verdict");
    if (x.attempts !== y.attempts) differs.push("attempts");
    if (exits(a, step).join() !== exits(b, step).join()) differs.push("exit codes");
    const cx = counts(x);
    const cy = counts(y);
    for (const port of [...new Set([...Object.keys(cx), ...Object.keys(cy)])].sort()) {
      if (cx[port] !== cy[port]) differs.push(`items on ${port}`);
    }
    if ((x.image ?? "") !== (y.image ?? "")) differs.push("image");
    if (readsData && canonical(paramsOf(a, step) ?? {}) !== canonical(paramsOf(b, step) ?? {})) differs.push("parameters");
    return { step, a: x, b: y, differs };
  });
}

// firstDifference is the first step the two runs part at, in the first run's order: where reading
// starts, since every step after it ran on what it published.
export function firstDifference(steps: StepDiff[]): string | undefined {
  return steps.find((s) => s.differs.length > 0)?.step;
}

// An item as two envelopes are compared on it: its data and its files, and not its id, which a
// runner mints where a script gave none and so differs between two runs of the same input.
const content = (i: Item) => canonical({ data: i.data, files: i.files });

// sameItems compares what two envelopes hold as two bags of items: how many each holds that the
// other holds too, and those only one of them holds, in the order they came.
export function sameItems(x: Envelope, y: Envelope): { same: number; onlyFirst: Item[]; onlySecond: Item[] } {
  const left = new Map<string, number>();
  for (const i of y.items) left.set(content(i), (left.get(content(i)) ?? 0) + 1);
  let same = 0;
  const onlyFirst: Item[] = [];
  for (const i of x.items) {
    const k = content(i);
    const n = left.get(k) ?? 0;
    if (n > 0) {
      left.set(k, n - 1);
      same++;
    } else {
      onlyFirst.push(i);
    }
  }
  const onlySecond: Item[] = [];
  for (const i of y.items) {
    const k = content(i);
    const n = left.get(k) ?? 0;
    if (n > 0) {
      left.set(k, n - 1);
      onlySecond.push(i);
    }
  }
  return { same, onlyFirst, onlySecond };
}

// candidates are the runs a run is offered to be read against: the others of its workflow and its
// commit that have ended, newest first, a succeeded one first where the run did not succeed, since
// that is the good run a failed one is read against.
export function candidates<T extends { run: string; commit: string; state: string; created_at: string }>(run: { run: string; commit: string; state: string }, listed: T[]): T[] {
  const ended = new Set(["succeeded", "failed", "cancelled", "timed_out"]);
  const others = listed.filter((r) => r.run !== run.run && r.commit === run.commit && ended.has(r.state)).sort((p, q) => q.created_at.localeCompare(p.created_at));
  if (run.state === "succeeded") return others;
  return [...others.filter((r) => r.state === "succeeded"), ...others.filter((r) => r.state !== "succeeded")];
}
