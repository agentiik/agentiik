import { fireEvent, render, screen } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { filtersOf } from "../src/lib/runs.svelte";
import { Session } from "../src/lib/session.svelte";
import { queryOfRange, rangeOf } from "../src/lib/stats";
import { answering, scenario } from "./scenario";

vi.mock("uplot", () => import("./plot"));

const now = Date.parse("2026-09-30T06:02:30Z");

describe("a statistics page's range", () => {
  it("is a preset counted back from now, the last 24 hours where the address names none", () => {
    const r = rangeOf(new URLSearchParams(""), now);
    expect(r.preset).toBe("24h");
    expect(r.to.getTime() - r.from.getTime()).toBe(86_400_000);
    expect(rangeOf(new URLSearchParams("range=7d&compare=previous"), now)).toMatchObject({ preset: "7d", compare: true });
  });

  it("is a span written out where a zoom wrote one, and is written back the same", () => {
    const q = new URLSearchParams("tab=quotas&from=2026-09-29T10:00:00.000Z&to=2026-09-29T14:00:00.000Z");
    const r = rangeOf(q, now);
    expect(r.preset).toBeUndefined();
    expect(queryOfRange(r, q).toString()).toBe("tab=quotas&from=2026-09-29T10%3A00%3A00.000Z&to=2026-09-29T14%3A00%3A00.000Z");
    expect(rangeOf(new URLSearchParams("from=2026-09-29T14:00:00Z&to=2026-09-29T10:00:00Z"), now).preset).toBe("24h");
  });
});

describe("the runs a bucket counts", () => {
  it("are the runs view narrowed to the bucket's bounds, both included", () => {
    const f = filtersOf(new URLSearchParams("since=2026-09-29T21:00:00Z&until=2026-09-29T21:59:59.999999999Z&span=7d"));
    expect(f.since).toBe("2026-09-29T21:00:00Z");
    expect(f.until).toBe("2026-09-29T21:59:59.999999999Z");
  });
});

function open(path: string, search = "") {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(scenario("alice"), asked));
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

describe("a namespace's statistics", () => {
  it("asks for its runs over the range the address names, with the span before where asked", async () => {
    const { asked } = open("/finance/statistics", "?range=7d&compare=previous");
    expect(await screen.findByText("Retries by exit code")).toBeTruthy();
    const series = asked.find((a) => a.startsWith("GET /api/v1/finance/stats/runs"));
    expect(series).toContain("compare=previous");
    const q = new URL(`http://x${series!.slice(4)}`).searchParams;
    expect(Date.parse(q.get("to")!) - Date.parse(q.get("from")!)).toBe(7 * 86_400_000);
  });

  it("opens no runs from a bucket, since a namespace's runs are listed under each workflow", async () => {
    const { place } = open("/finance/statistics");
    const [chart] = await screen.findAllByRole("slider");
    await fireEvent.keyDown(chart!, { key: "ArrowRight" });
    await fireEvent.keyDown(chart!, { key: "Enter" });
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "statistics" });
  });

  it("holds each chart's numbers in a table, and the namespace's quotas beside its load", async () => {
    open("/finance/statistics", "?tab=quotas");
    expect(await screen.findByText("max_concurrent_tasks")).toBeTruthy();
    const tables = await screen.findAllByRole("table");
    expect(tables.length).toBeGreaterThanOrEqual(4);
    expect(screen.getAllByText("2026-09-29 07:00 to 08:00 UTC").length).toBeGreaterThan(0);
  });

  it("is not offered in a namespace the caller holds nothing in", async () => {
    open("/payroll/statistics");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });
});
