import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { chosen, exitCodes, figures, heat, notSucceeded, refused, type PortsSeries, type RunsBucket, type RunsSeries, type StepSeries } from "../src/lib/workflow-stats";
import { answering, recordedFor, scenario } from "./scenario";

vi.mock("uplot", () => import("./plot"));

function bucket(runs: Partial<RunsBucket["runs"]>, more: Partial<RunsBucket> = {}): RunsBucket {
  return {
    since: "2026-09-01T00:00:00Z",
    until: "2026-09-01T23:59:59.999999999Z",
    runs: { queued: 0, running: 0, waiting: 0, succeeded: 0, failed: 0, cancelled: 0, timed_out: 0, ...runs },
    retries: [],
    ...more,
  };
}

describe("a workflow's figures", () => {
  const runs: RunsSeries = {
    from: "2026-09-01T00:00:00Z",
    to: "2026-10-01T00:00:00Z",
    bucket: "1d",
    buckets: [],
    overall: bucket({ succeeded: 296, failed: 12, timed_out: 2, running: 3 }, { duration_ms: { p50: 227000, p95: 372000, p99: 458000 }, queue_wait_ms: { p50: 600, p95: 3400, p99: 4100 } }),
    previous: {
      from: "2026-08-02T00:00:00Z",
      to: "2026-09-01T00:00:00Z",
      buckets: [],
      overall: bucket({ succeeded: 268, failed: 20, timed_out: 2 }, { duration_ms: { p50: 230000, p95: 421000, p99: 470000 }, queue_wait_ms: { p50: 500, p95: 2800, p99: 3000 } }),
    },
  };

  it("are read from the range as one bucket, and changed against the span before where the page compares", () => {
    const [count, share, duration, wait] = figures(runs, undefined, true);
    expect(count).toEqual({ label: "Runs", value: "313", change: "+8%", aside: "290 in the span before" });
    // 296 of the 310 that ended, against 268 of 290: a running run is no failure yet.
    expect(share).toMatchObject({ value: "95.5%", change: "+3.1 pts", aside: "296 of 310 that ended; 12 failed, 2 timed out" });
    expect(duration).toMatchObject({ value: "6m 12s", change: "−49s", aside: "p50 3m 47s · p99 7m 38s" });
    expect(wait).toMatchObject({ value: "3s", change: "+600ms" });
  });

  it("carry no change where the page does not compare, and none from nothing", () => {
    const [count, share] = figures(runs, undefined, false);
    expect(count).toEqual({ label: "Runs", value: "313", change: undefined, aside: "created over the range" });
    expect(share!.change).toBeUndefined();
    const none = figures({ ...runs, overall: bucket({}) }, undefined, false);
    expect(none.map((f) => f.value)).toEqual(["0", "none ended", "none", "none"]);
  });

  it("end with the step's throughput where it fans out, and its p95 where it does not", () => {
    const fan: StepSeries = { step: "invoice", overall: { since: "", until: "", attempts: 90, exit_codes: [], items_per_minute: 212 }, previous_overall: { since: "", until: "", attempts: 80, exit_codes: [], items_per_minute: 178 } };
    expect(figures(runs, fan, true).at(-1)).toEqual({ label: "invoice throughput", value: "212 items/min", change: "+19%", aside: "while its shards run" });
    const one: StepSeries = { step: "normalize", overall: { since: "", until: "", attempts: 21, exit_codes: [], duration_ms: { p50: 1150, p95: 1800, p99: 2050 } } };
    expect(figures(runs, one, false).at(-1)).toMatchObject({ label: "normalize, p95", value: "1s", aside: "p50 1s, 21 attempts" });
  });
});

describe("a workflow's charts", () => {
  it("share out the runs that ended and did not succeed, none where none ended", () => {
    const s = notSucceeded([bucket({ succeeded: 12, failed: 5, running: 3 }), bucket({ running: 2 })]);
    expect(s.failed[0]).toBeCloseTo((5 / 17) * 100);
    expect(s.all).toEqual([s.failed[0], null]);
  });

  it("count a step's attempts that failed by code, a lost one under null, success left out", () => {
    const codes = exitCodes([
      { since: "", until: "", attempts: 10, exit_codes: [{ exit_code: 0, attempts: 7 }, { exit_code: 108, attempts: 2 }, { exit_code: null, attempts: 1 }] },
      { since: "", until: "", attempts: 5, exit_codes: [{ exit_code: 108, attempts: 1 }, { exit_code: 1, attempts: 1 }] },
    ]);
    expect(codes).toEqual([
      { code: 108, attempts: 3 },
      { code: 1, attempts: 1 },
      { code: null, attempts: 1 },
    ]);
  });

  it("are of the step the address names, or else the one whose attempts failed most", () => {
    const steps: StepSeries[] = [
      { step: "normalize", buckets: [{ since: "", until: "", attempts: 3, exit_codes: [{ exit_code: 1, attempts: 1 }] }] },
      { step: "invoice", buckets: [{ since: "", until: "", attempts: 9, exit_codes: [{ exit_code: 108, attempts: 4 }] }] },
    ];
    expect(chosen(steps, null)?.step).toBe("invoice");
    expect(chosen(steps, "normalize")?.step).toBe("normalize");
    expect(chosen(steps, "gone")?.step).toBe("invoice");
    expect(chosen([{ step: "a", buckets: [] }, { step: "b", buckets: [] }], null)?.step).toBe("a");
  });

  it("lay a heatmap's medians in five strengths from the shortest to the longest", () => {
    const h = heat([100, 200, undefined, 600]);
    expect(h.levels).toEqual([100, 200, 300, 400, 500]);
    expect([h.level(100), h.level(250), h.level(599), h.level(600), h.level(undefined)]).toEqual([1, 2, 5, 5, 0]);
    expect(heat([undefined]).levels).toEqual([]);
  });

  it("share out each step's items rejected or sent to error, leaving out a step that sent none there", () => {
    const ports: PortsSeries = {
      from: "",
      to: "",
      bucket: "1d",
      workflow: "monthly-invoicing",
      steps: [
        { step: "normalize", buckets: [{ since: "a", until: "a", items: { ok: 97, rejected: 3 } }, { since: "b", until: "b", items: { ok: 0, rejected: 0 } }] },
        { step: "invoice", buckets: [{ since: "a", until: "a", items: { out: 50, error: 0 } }, { since: "b", until: "b", items: { out: 10, error: 0 } }] },
      ],
    };
    const r = refused(ports);
    expect(r.lines).toEqual([{ step: "normalize", port: "rejected", values: [3, null] }]);
    expect([r.rejected, r.error, r.of]).toEqual([3, 0, 100]);
  });
});

describe("a recorded scenario", () => {
  it("answers a request by the part of its query it was recorded under, the most of it first", () => {
    const s = { "GET /x": 1, "GET /x?workflow=w": 2, "GET /x?workflow=w&by=hour": 3 };
    expect(recordedFor(s, "GET", "/x", "?from=a&workflow=w&by=hour")).toBe(3);
    expect(recordedFor(s, "GET", "/x", "?workflow=w&from=a")).toBe(2);
    expect(recordedFor(s, "GET", "/x", "?workflow=v")).toBe(1);
  });
});

function open(path: string, search = "") {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(scenario("alice"), asked));
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

describe("a workflow's statistics page", () => {
  it("asks the four routes for the workflow over the range, the heatmap without the span before", async () => {
    const { asked } = open("/finance/workflows/monthly-invoicing/statistics", "?range=30d&compare=previous");
    expect(await screen.findByText("Runs that did not succeed")).toBeTruthy();
    const of = (route: string) => asked.filter((a) => a.startsWith(`GET /api/v1/finance/stats/${route}`)).map((a) => new URL(`http://x${a.slice(4)}`).searchParams);
    for (const route of ["runs", "steps", "ports"]) {
      expect(of(route).some((q) => q.get("workflow") === "monthly-invoicing" && q.get("compare") === "previous")).toBe(true);
    }
    const byHour = of("steps").find((q) => q.get("by") === "hour");
    expect(byHour?.get("compare")).toBeNull();
    expect(Date.parse(byHour!.get("to")!) - Date.parse(byHour!.get("from")!)).toBe(30 * 86_400_000);
  });

  it("draws the figures, and the step whose attempts failed most with its exit codes and its week", async () => {
    open("/finance/workflows/monthly-invoicing/statistics", "?range=30d&compare=previous");
    expect(await screen.findByText("Exit codes of invoice")).toBeTruthy();
    expect(screen.getByText("invoice throughput")).toBeTruthy();
    expect(screen.getByText("296")).toBeTruthy();
    expect(screen.getByText("322 in the span before")).toBeTruthy();
    const codes = screen.getByText("transient failure").closest("ul")!;
    expect(within(codes).getAllByRole("listitem").map((li) => li.querySelector(".code")?.textContent)).toEqual(["108", "120", "1", "137", "lost"]);
    const week = screen.getByRole("table", { name: /p50 duration of invoice by weekday and hour/ });
    expect(within(week).getAllByRole("row")).toHaveLength(8);
    expect(screen.getByText(/^[\d,]+ rejected, [\d,]+ to error, of [\d,]+$/)).toBeTruthy();
  });

  it("writes the step chosen into the address, and opens the runs of a bucket narrowed to the workflow", async () => {
    const { place } = open("/finance/workflows/monthly-invoicing/statistics", "?range=30d");
    const select = (await screen.findByLabelText("Step")) as HTMLSelectElement;
    await fireEvent.change(select, { target: { value: "normalize" } });
    expect(place.query.get("step")).toBe("normalize");
    expect(place.query.get("range")).toBe("30d");
    expect(await screen.findByText("Exit codes of normalize")).toBeTruthy();

    const [chart] = await screen.findAllByRole("slider");
    await fireEvent.keyDown(chart!, { key: "ArrowRight" });
    await fireEvent.keyDown(chart!, { key: "Enter" });
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing", tab: "runs" });
    expect(place.query.get("since")).toBe("2026-09-01T00:00:00Z");
  });

  it("is reached from the workflow's runs, and offered nowhere the caller reads no runs", async () => {
    const { place } = open("/finance/workflows/monthly-invoicing/runs");
    await fireEvent.click(within(await screen.findByRole("navigation", { name: "monthly-invoicing, what is shown" })).getByRole("link", { name: "Statistics" }));
    expect(place.route).toMatchObject({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing", tab: "statistics" });

    open("/payroll/workflows/monthly-invoicing/statistics");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });
});
