import type { RunDetail } from "./run.svelte";

// The run inspector's timeline: a lane for each step of the run, and on it a span for each attempt,
// from its first shard's start to its last shard's end, so that a reader sees where the run's time
// went and what a retry cost. An attempt that failed and was retried is drawn paler than the one the
// step ended on, which alone carries the step's verdict.

export type Tone = "succeeded" | "failed" | "running" | "cancelled";

export interface Span {
  attempt: number;
  // from and to are milliseconds from the run's start.
  from: number;
  to: number;
  tone: Tone;
  // retried is an attempt another came after.
  retried: boolean;
}

export interface Lane {
  step: string;
  spans: Span[];
}

export interface Timeline {
  start: number;
  // length is the run's span in milliseconds, which every lane is drawn against.
  length: number;
  lanes: Lane[];
}

const going = new Set(["pending", "dispatched", "running", "publishing"]);

// toneOf is an attempt's colour from the states of its shards: still going while any is, failed
// where any failed, timed out or was lost, cancelled where any was cancelled, and succeeded else.
function toneOf(states: string[]): Tone {
  if (states.some((s) => going.has(s))) return "running";
  if (states.some((s) => s === "failed" || s === "timed_out" || s === "lost")) return "failed";
  if (states.some((s) => s === "cancelled")) return "cancelled";
  return "succeeded";
}

// timeline is the run's timeline at now: its span from the run's start to its end, or to now while
// it runs, and each step's attempts on it, in the order the run lists its steps. A task not started
// yet draws nothing, and a step none of whose tasks started is a lane with no span.
export function timeline(run: RunDetail, now: number): Timeline {
  const starts = run.tasks.map((t) => (t.started_at ? Date.parse(t.started_at) : NaN)).filter((n) => !Number.isNaN(n));
  const start = run.started_at ? Date.parse(run.started_at) : starts.length ? Math.min(...starts) : now;
  const end = run.finished_at ? Date.parse(run.finished_at) : now;
  const lanes = run.steps.map((s): Lane => {
    const byAttempt = new Map<number, { from: number; to: number; states: string[] }>();
    for (const t of run.tasks) {
      if (t.step !== s.step || !t.started_at) continue;
      const from = Date.parse(t.started_at);
      const to = t.finished_at ? Date.parse(t.finished_at) : end;
      const held = byAttempt.get(t.attempt);
      if (held) {
        held.from = Math.min(held.from, from);
        held.to = Math.max(held.to, to);
        held.states.push(t.state);
      } else {
        byAttempt.set(t.attempt, { from, to, states: [t.state] });
      }
    }
    const last = Math.max(0, ...byAttempt.keys());
    const spans = [...byAttempt.entries()]
      .sort(([a], [b]) => a - b)
      .map(([attempt, a]): Span => ({ attempt, from: Math.max(0, a.from - start), to: Math.max(0, a.to - start), tone: toneOf(a.states), retried: attempt < last }));
    return { step: s.step, spans };
  });
  return { start, length: Math.max(1, end - start), lanes };
}

const steps = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400].map((s) => s * 1000);

// ticks are the marks under the lanes: round instants from the run's start, at most five of them
// before its end, and the end itself, unless it falls so close to the last mark that the two words
// would run into each other.
export function ticks(length: number): { at: number; label: string }[] {
  const step = steps.find((s) => length / s <= 4) ?? steps[steps.length - 1]!;
  const out: { at: number; label: string }[] = [];
  for (let at = 0; at <= length; at += step) out.push({ at, label: mark(at) });
  const last = out[out.length - 1]!;
  if (length - last.at >= step * 0.6) out.push({ at: length, label: mark(length) });
  return out;
}

// mark is a span from the run's start as a clock reads it, m:ss, or h:mm:ss past the hour.
export function mark(ms: number): string {
  const s = Math.round(ms / 1000);
  const two = (n: number) => String(n).padStart(2, "0");
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return h > 0 ? `${h}:${two(m)}:${two(s % 60)}` : `${m}:${two(s % 60)}`;
}
