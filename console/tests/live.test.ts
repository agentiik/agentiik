import { render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Live } from "../src/lib/live.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { opened, sockets } from "./live";
import { answering, scenario } from "./scenario";

function open(path: string, who: "alice" | "dana" = "alice") {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(scenario(who), asked));
  const place = new Place({ pathname: path, baseURI: "https://agentiik.example.com/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return asked;
}

// count is how many times a route was asked for, its query aside.
const count = (asked: string[], route: string) => asked.filter((a) => a.split("?")[0] === route).length;

afterEach(() => vi.useRealTimers());

describe("the live connection", () => {
  it("is opened on the console's own origin, as wss where the console is served over https", () => {
    new Live("https://agentiik.example.com/console/").start();
    expect(sockets.at(-1)!.url).toBe("wss://agentiik.example.com/console/api/v1/me/live");
    new Live("http://localhost:8443/").start();
    expect(sockets.at(-1)!.url).toBe("ws://localhost:8443/api/v1/me/live");
  });

  it("reads once a burst of changes has settled, everything on opening, and connects again longer apart each time", () => {
    vi.useFakeTimers();
    let ended = 0;
    const live = new Live("https://agentiik.example.com/", () => ended++);
    const reads: string[] = [];
    live.when((c) => c.kind === "run" && c.namespace === "finance", () => reads.push("finance"));
    live.start();
    const first = opened();
    expect(live.open).toBe(true);
    vi.advanceTimersByTime(250);
    expect(reads).toEqual(["finance"]);

    for (let i = 0; i < 5; i++) first.say({ kind: "run", namespace: "finance", workflow: "monthly-invoicing", run: "01JMZ8W4K2R7AAAAAAAAAAAAAA" });
    first.say({ kind: "run", namespace: "team-ops", workflow: "monthly-invoicing", run: "01JMZ8W4K2R7AAAAAAAAAAAAAB" });
    vi.advanceTimersByTime(250);
    expect(reads).toEqual(["finance", "finance"]);

    first.end(1006);
    expect(live.open).toBe(false);
    expect(sockets).toHaveLength(1);
    vi.advanceTimersByTime(1000);
    expect(sockets).toHaveLength(2);
    sockets[1]!.end(1006);
    vi.advanceTimersByTime(1999);
    expect(sockets).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(sockets).toHaveLength(3);

    // A credential the API no longer takes reads who the console is signed in as.
    sockets[2]!.open();
    sockets[2]!.end(1008);
    expect(ended).toBe(1);
    // Stopped, it connects no more.
    live.stop();
    vi.advanceTimersByTime(60_000);
    expect(sockets).toHaveLength(3);
  });
});

describe("the console, live", () => {
  it("says it is live once the connection opens, and not live while it is not", async () => {
    open("/finance/runs");
    const word = await screen.findByText("reconnecting");
    expect(word.closest("[role=status]")).toBeTruthy();
    opened();
    expect(await screen.findByText("live")).toBeTruthy();
    sockets.at(-1)!.end(1006);
    expect(await screen.findByText("reconnecting")).toBeTruthy();
  });

  it("reads the runs again when one of the namespace's changes, and not for another namespace's", async () => {
    const asked = open("/finance/runs");
    await screen.findByText("reconnecting");
    await waitFor(() => expect(count(asked, "GET /api/v1/runs")).toBe(1));
    const s = opened();
    await waitFor(() => expect(count(asked, "GET /api/v1/runs")).toBe(2));
    s.say({ kind: "run", namespace: "team-ops", workflow: "monthly-invoicing", run: "01JMZ8W4K2R7AAAAAAAAAAAAAB" });
    await new Promise((r) => setTimeout(r, 400));
    expect(count(asked, "GET /api/v1/runs")).toBe(2);
    s.say({ kind: "run", namespace: "finance", workflow: "monthly-invoicing", run: "01JMZ8W4K2R7AAAAAAAAAAAAAA" });
    await waitFor(() => expect(count(asked, "GET /api/v1/runs")).toBe(3));
  });

  it("reads who it is signed in as again when the caller's notifications change", async () => {
    const asked = open("/finance/runs");
    await screen.findByText("reconnecting");
    const s = opened();
    await waitFor(() => expect(count(asked, "GET /api/v1/me")).toBe(2));
    s.say({ kind: "notifications" });
    await waitFor(() => expect(count(asked, "GET /api/v1/me")).toBe(3));
  });

  it("reads the installation's activity again on an administrator's home when a run changes anywhere, at most every two seconds", async () => {
    const asked = open("/", "dana");
    await screen.findByText("reconnecting");
    await waitFor(() => expect(count(asked, "GET /api/v1/stats/activity")).toBe(1));
    const s = opened();
    // Opening reads everything again, two seconds after the first read at the soonest.
    await waitFor(() => expect(count(asked, "GET /api/v1/stats/activity")).toBe(2), { timeout: 4000 });
    s.say({ kind: "activity" });
    s.say({ kind: "activity" });
    await waitFor(() => expect(count(asked, "GET /api/v1/stats/activity")).toBe(3), { timeout: 4000 });
    await new Promise((r) => setTimeout(r, 300));
    expect(count(asked, "GET /api/v1/stats/activity")).toBe(3);
  }, 15000);

  it("reads the runners again when one changes, to an administrator", async () => {
    const asked = open("/runners", "dana");
    await screen.findByText("reconnecting");
    await waitFor(() => expect(count(asked, "GET /api/v1/runners")).toBe(1));
    const s = opened();
    await waitFor(() => expect(count(asked, "GET /api/v1/runners")).toBe(2));
    s.say({ kind: "runners" });
    await waitFor(() => expect(count(asked, "GET /api/v1/runners")).toBe(3));
  });
});
