import { afterEach, describe, expect, it, vi } from "vitest";
import { chartOf, created, hour, paced, type Activity } from "../src/lib/installation";
import { scenario } from "./scenario";

const activity = scenario("dana")["GET /api/v1/stats/activity"]!.body as Activity;

describe("the installation's activity", () => {
  afterEach(() => vi.useRealTimers());

  it("is asked for the last hour, to the end of the current minute, which it draws as it fills", () => {
    expect(hour(Date.parse("2026-10-01T06:02:30Z"))).toEqual({ from: "2026-10-01T05:03:00.000Z", to: "2026-10-01T06:03:00.000Z" });
    expect(hour(Date.parse("2026-10-01T06:03:00Z"))).toEqual({ from: "2026-10-01T05:04:00.000Z", to: "2026-10-01T06:04:00.000Z" });
  });

  it("draws the runs created as columns by where they stand, those still going together, and the tasks in flight as a line", () => {
    const { since, series } = chartOf(activity);
    expect(since).toHaveLength(60);
    expect(series.map((s) => `${s.label}/${s.kind}`)).toEqual(["succeeded/bars", "failed/bars", "timed_out/bars", "cancelled/bars", "running/bars", "tasks/step"]);
    const last = activity.buckets[59]!;
    expect(series[4]!.values[59]).toBe(last.runs.queued + last.runs.running + last.runs.waiting);
    expect(series[5]!.values[59]).toBe(last.tasks_in_flight_max);
    const all = activity.buckets.reduce((n, b) => n + Object.values(b.runs).reduce((m, v) => m + v, 0), 0);
    expect(created(activity)).toBe(all);
  });

  it("is read when asked, at most once every gap, a change that comes sooner read once at the gap's end", () => {
    vi.useFakeTimers();
    let at = 0;
    let reads = 0;
    const pace = paced(() => reads++, 2000, { now: () => at });
    pace.ask();
    expect(reads).toBe(1);
    at = 500;
    pace.ask();
    pace.ask();
    expect(reads).toBe(1);
    at = 2000;
    vi.advanceTimersByTime(1500);
    expect(reads).toBe(2);
    at = 6000;
    pace.ask();
    expect(reads).toBe(3);
    at = 6100;
    pace.ask();
    pace.stop();
    vi.advanceTimersByTime(5000);
    expect(reads).toBe(3);
  });
});
