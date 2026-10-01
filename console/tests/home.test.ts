import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { added, boundsOf, dayOf, grid, mondayOf, months, none, said, shades, weeks, yearOf, type RunsSeries } from "../src/lib/activity";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

vi.mock("uplot", () => import("./plot"));

const now = Date.parse("2026-10-01T06:02:30Z"); // a Thursday

describe("a year of activity", () => {
  it("is 53 columns of a week each, from Monday, ending with the current week", () => {
    expect(dayOf(mondayOf(now))).toBe("2026-09-28");
    expect(dayOf(mondayOf(Date.parse("2026-09-28T00:00:00Z")))).toBe("2026-09-28");
    expect(dayOf(mondayOf(Date.parse("2026-10-04T23:59:59Z")))).toBe("2026-09-28");
    const { from, to } = yearOf(now);
    expect(from.toISOString()).toBe("2025-09-29T00:00:00.000Z");
    expect(to.getTime()).toBe(now);
    const columns = grid(now, new Map());
    expect(columns).toHaveLength(weeks);
    expect(columns.every((c) => c.length === 7)).toBe(true);
    expect(columns[0]![0]!.day).toBe("2025-09-29");
    // The days of the current week after today are to come.
    expect(columns[52]!.map((s) => s.future)).toEqual([false, false, false, false, true, true, true]);
  });

  it("adds every namespace's series by the day each bucket opens", () => {
    const bucket = (since: string, succeeded: number, failed = 0) => ({ since, until: since.replace("00:00:00Z", "23:59:59.999999999Z"), runs: { ...none(), succeeded, failed }, retries: [] });
    const a = { buckets: [bucket("2026-09-29T00:00:00Z", 3), bucket("2026-09-30T00:00:00Z", 1, 1)] } as unknown as RunsSeries;
    const b = { buckets: [bucket("2026-09-30T00:00:00Z", 4)] } as unknown as RunsSeries;
    const counts = added([a, b]);
    expect(counts.get("2026-09-29")).toMatchObject({ succeeded: 3 });
    expect(counts.get("2026-09-30")).toMatchObject({ succeeded: 5, failed: 1 });
    const square = grid(now, counts).flat().find((s) => s.day === "2026-09-30")!;
    expect(said(square, "runs")).toBe("6 runs on Wednesday, 30 September 2026: 5 succeeded, 1 failed");
    expect(said(square, "failures")).toBe("1 failure on Wednesday, 30 September 2026");
  });

  it("shades in quarters of the days that counted something, so one busy day leaves the rest visible", () => {
    const s = shades([0, 1, 1, 2, 2, 3, 4, 5, 6, 7, 8, 1000]);
    expect(s.level(0)).toBe(0);
    expect(s.level(1)).toBe(1);
    expect(s.level(1000)).toBe(4);
    // A scale up to the busiest day would draw every other day in the palest shade.
    expect(s.level(7)).toBe(4);
    expect(s.level(3)).toBeGreaterThan(1);
    expect(shades([0, 0]).level(5)).toBe(0);
    // Every day alike is drawn alike.
    expect(new Set([1, 1, 1].map(shades([1, 1, 1]).level))).toEqual(new Set([1]));
  });

  it("names a month over its first column, never squeezed against the one before", () => {
    const labels = months(grid(now, new Map()));
    // September holds one column of the year, and gives way to October.
    expect(labels[0]).toEqual({ column: 1, name: "Oct" });
    expect(labels.slice(1).every((l, i) => l.column - labels[i]!.column >= 3)).toBe(true);
    expect(labels.map((l) => l.name)).toContain("Mar");
  });

  it("opens a day's runs with the bounds of its bucket, both included", () => {
    expect(boundsOf("2026-09-30")).toEqual({ since: "2026-09-30T00:00:00Z", until: "2026-09-30T23:59:59.999999999Z" });
  });
});

function open(search = "", who: "alice" | "dana" = "alice") {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(scenario(who), asked));
  const place = new Place({ pathname: "/", search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

describe("the home", () => {
  // The squares follow the clock: it is held on a Thursday, only the date being faked, so that what
  // the console waits on still runs.
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("reads each namespace the caller reads runs in by the day, over the year", async () => {
    const { asked } = open();
    const activity = await screen.findByRole("region", { name: "Activity" });
    await within(activity).findByText(/runs in 12 months/);
    const series = asked.filter((a) => a.includes("/stats/runs"));
    expect(series.map((a) => a.split("?")[0])).toEqual(["GET /api/v1/alice/stats/runs", "GET /api/v1/finance/stats/runs", "GET /api/v1/team-ops/stats/runs"]);
    for (const a of series) {
      const q = new URL(`http://x${a.slice(4)}`).searchParams;
      expect(q.get("bucket")).toBe("1d");
      expect(new Date(q.get("from")!).getUTCDay()).toBe(1);
      expect(q.get("from")!.slice(11)).toBe("00:00:00.000Z");
    }
    // Every day of the year so far is offered, the three to come this week drawn and not offered.
    expect(within(activity).getAllByRole("gridcell").length).toBe(weeks * 7 - 3);
  });

  it("lists the namespaces, the workflows last run, and the latest runs across them by the day", async () => {
    const { asked } = open();
    const spaces = await screen.findByRole("region", { name: "Namespaces" });
    expect(within(spaces).getAllByRole("link").map((l) => l.textContent)).toEqual(["alice", "finance", "team-ops"]);
    expect(within(spaces).getByRole("link", { name: "finance" }).getAttribute("href")).toBe("/finance/workflows");
    const last = screen.getByRole("region", { name: "Latest runs" });
    expect(await within(last).findAllByText(/^finance\//)).not.toHaveLength(0);
    expect(within(last).getByRole("region", { name: "Yesterday" })).toBeTruthy();
    const flows = screen.getByRole("region", { name: "Workflows" });
    expect(within(flows).getByRole("link", { name: "finance/monthly-invoicing" }).getAttribute("href")).toBe("/finance/workflows/monthly-invoicing");
    expect(asked).toContain("GET /api/v1/runs?limit=50");
  });

  it("counts what runs and awaits approval now, and lists what needs the caller", async () => {
    const { asked } = open();
    const cards = await screen.findByRole("list", { name: "Counts" });
    await waitFor(() => expect(within(cards).getByText("Running").closest("li")!.textContent!.replace(/\s+/g, "")).toMatch(/^Running\d+now$/));
    expect(asked.some((a) => a.startsWith("GET /api/v1/runs?state=waiting"))).toBe(true);
    const attention = screen.getByRole("region", { name: "Needs your attention" });
    const show = within(attention).getByRole("group", { name: "Show" });
    await fireEvent.click(within(show).getByRole("button", { name: "Failed" }));
    expect(within(show).getByRole("button", { name: "Failed" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("opens a day's runs across every namespace, as ?day= in the address, and closes it", async () => {
    const { asked, place } = open();
    const activity = await screen.findByRole("region", { name: "Activity" });
    await within(activity).findByText(/ in 12 months/);
    const day = within(activity).getByRole("gridcell", { name: /Tuesday, 29 September 2026/ });
    await fireEvent.click(day);
    expect(place.query.get("day")).toBe("2026-09-29");
    expect(await within(activity).findByRole("region", { name: "Runs of 2026-09-29" })).toBeTruthy();
    expect(asked).toContain("GET /api/v1/runs?since=2026-09-29T00%3A00%3A00Z&until=2026-09-29T23%3A59%3A59.999999999Z&limit=101");
    await fireEvent.click(within(activity).getByRole("button", { name: "Close" }));
    expect(place.query.get("day")).toBeNull();
  });

  it("moves the day chosen with the arrows, a day up and down and a week left and right", async () => {
    const { place } = open("?day=2026-09-23");
    await screen.findByRole("region", { name: "Runs of 2026-09-23" });
    await fireEvent.keyDown(window, { key: "ArrowDown" });
    expect(place.query.get("day")).toBe("2026-09-24");
    await fireEvent.keyDown(window, { key: "ArrowRight" });
    expect(place.query.get("day")).toBe("2026-10-01");
    // Never onto a day to come.
    await fireEvent.keyDown(window, { key: "ArrowDown" });
    expect(place.query.get("day")).toBe("2026-10-01");
    await fireEvent.keyDown(window, { key: "ArrowLeft" });
    expect(place.query.get("day")).toBe("2026-09-24");
    await fireEvent.keyDown(window, { key: "Escape" });
    expect(place.query.get("day")).toBeNull();
  });

  it("shows an administrator what the installation is doing, the last hour a minute at a time", async () => {
    const { asked } = open("", "dana");
    const installation = await screen.findByRole("region", { name: "Server activity" });
    await within(installation).findByText("121 runs in the last hour");
    const now = within(installation).getByRole("list", { name: "Now" });
    const figure = (label: string) => within(now).getByText(label).parentElement!.textContent;
    expect(figure("Runs running")).toBe("Runs running7");
    expect(figure("Tasks running")).toBe("Tasks running17 / 24");
    expect(figure("Runners ready")).toBe("Runners ready3 / 4");
    expect(within(installation).getByRole("figure")).toBeTruthy();
    const q = new URL(`http://x${asked.find((a) => a.startsWith("GET /api/v1/stats/activity"))!.slice(4)}`).searchParams;
    expect([q.get("from"), q.get("to"), q.get("bucket")]).toEqual(["2026-10-01T05:03:00.000Z", "2026-10-01T06:03:00.000Z", "1m"]);
  });

  it("shows a user who administers nothing no figure of the installation", async () => {
    const { asked } = open();
    await screen.findByRole("region", { name: "Activity" });
    expect(screen.queryByRole("region", { name: "Server activity" })).toBeNull();
    expect(asked.some((a) => a.startsWith("GET /api/v1/stats/activity"))).toBe(false);
  });

  it("shades by the runs that failed where asked", async () => {
    const { place } = open();
    const activity = await screen.findByRole("region", { name: "Activity" });
    await within(activity).findByText(/ in 12 months/);
    await fireEvent.click(within(activity).getByRole("button", { name: "Failures" }));
    expect(place.query.get("by")).toBe("failures");
    expect(await within(activity).findByText(/failures in 12 months/)).toBeTruthy();
  });
});
