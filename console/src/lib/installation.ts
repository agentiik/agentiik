import type { components } from "../api/schema";
import type { Series } from "../components/Chart.svelte";

// What every namespace together is doing, as GET /api/v1/stats/activity answers it to an
// administrator, and the chart the home draws of it.

export type Activity = components["schemas"]["statsActivity"];

// hour is the range the home asks for: the last hour, from the start of the minute an hour back to
// the end of the current one, so that the current minute is a bucket the chart draws as it fills.
export function hour(now: number): { from: string; to: string } {
  const minute = 60_000;
  const end = Math.floor(now / minute) * minute + minute;
  return { from: new Date(end - 60 * minute).toISOString(), to: new Date(end).toISOString() };
}

// chartOf is the chart of the last hour: the runs created each minute as columns by where they
// stand, those still going together, and the most tasks in flight at once as a line over them.
export function chartOf(a: Activity): { since: string[]; series: Series[] } {
  const b = a.buckets;
  const going = b.map((x) => x.runs.queued + x.runs.running + x.runs.waiting);
  return {
    since: b.map((x) => x.since),
    series: [
      { label: "succeeded", tone: "succeeded", kind: "bars", values: b.map((x) => x.runs.succeeded) },
      { label: "failed", tone: "failed", kind: "bars", values: b.map((x) => x.runs.failed) },
      { label: "timed_out", tone: "waiting", kind: "bars", values: b.map((x) => x.runs.timed_out) },
      { label: "cancelled", tone: "quiet", kind: "bars", values: b.map((x) => x.runs.cancelled) },
      { label: "running", tone: "running", kind: "bars", values: going },
      { label: "tasks", tone: "accent", kind: "step", values: b.map((x) => x.tasks_in_flight_max) },
    ],
  };
}

// created is how many runs the hour created, whatever they came to.
export function created(a: Activity): number {
  return a.buckets.reduce((n, x) => n + Object.values(x.runs).reduce((m, v) => m + v, 0), 0);
}

// paced calls read when asked, at most once every gap: a change that comes sooner is read at the end
// of the gap, once however many came, so that a busy installation is read a few times a minute
// rather than at every change its live connection tells of.
export function paced(read: () => void, gap: number, clock: { now: () => number } = Date): { ask: () => void; stop: () => void } {
  let last = Number.NEGATIVE_INFINITY;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const run = () => {
    timer = undefined;
    last = clock.now();
    read();
  };
  return {
    ask() {
      if (timer !== undefined) return;
      const wait = last + gap - clock.now();
      if (wait <= 0) run();
      else timer = setTimeout(run, wait);
    },
    stop() {
      clearTimeout(timer);
      timer = undefined;
    },
  };
}
