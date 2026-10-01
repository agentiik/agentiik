import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { moved, shown } from "../src/lib/keys.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

const failed = "01JMZ8V1P9C4XQ7K2N4D6F8H0A";

function open(path: string, s: Scenario = scenario("alice")) {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(s, asked));
  const [pathname, search] = path.split("?");
  const place = new Place({ pathname: pathname!, search: search ? `?${search}` : "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

// The key line, as a list of what each key does, in the words it names them with.
function line(): string[] {
  return within(screen.getByRole("list", { name: "Keys of this view" }))
    .getAllByRole("listitem")
    .map((li) => li.textContent!.replace(/\s+/g, " ").trim());
}

const press = (key: string, target: Element | Window = window) => fireEvent.keyDown(target, { key });

describe("the key line", () => {
  it("names the keys of the runs view by their effect, and the console's own", async () => {
    open("/finance/runs");
    await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A");
    expect(line()).toEqual(["↑↓ Move", "enter Open", "1234 Runs, workflows, statistics, settings", "? Every key"]);
  });

  it("moves the selection over the runs and opens the one selected", async () => {
    const { place } = open("/finance/runs");
    await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A");
    const rows = () => screen.getAllByRole("row").filter((r) => r.hasAttribute("data-run"));
    await press("ArrowDown");
    expect(rows()[0]!.getAttribute("aria-selected")).toBe("true");
    await press("j");
    expect(rows()[1]!.getAttribute("aria-selected")).toBe("true");
    expect(rows()[0]!.getAttribute("aria-selected")).toBe("false");
    await press("k");
    await press("k");
    expect(rows()[0]!.getAttribute("aria-selected")).toBe("true");
    const id = rows()[0]!.getAttribute("data-run")!;
    await press("Enter");
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: id });
  });

  it("leaves a key typed into a field to the field", async () => {
    open("/finance/runs");
    await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A");
    await press("j", screen.getByRole("switch"));
    expect(screen.getAllByRole("row").some((r) => r.getAttribute("aria-selected") === "true")).toBe(false);
  });

  it("leaves enter on a focused link to the link", async () => {
    const { place } = open("/finance/runs");
    await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A");
    await press("ArrowDown");
    await press("Enter", screen.getByRole("link", { name: "Statistics" }));
    expect(place.route.kind === "namespace" && place.route.run).toBeFalsy();
  });

  it("opens a view of the top bar by its number", async () => {
    const { place } = open(`/finance/runs/${failed}`);
    await screen.findByText("invoice · shard 3/8 · attempt 2");
    await press("1");
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: undefined });
  });

  it("lists every key of the view with ?, and closes the list with esc", async () => {
    const { place } = open("/finance/runs");
    await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A");
    await press("?");
    const listing = screen.getByRole("dialog", { name: "Every key of this view" });
    expect(within(listing).getByText("Move").closest("tr")!.textContent!.replace(/\s+/g, "")).toBe("↑↓kjMove");
    await press("Escape");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(place.route).toMatchObject({ view: "runs" });
  });
});

describe("the inspector's keys", () => {
  it("moves between the steps and goes back to the runs", async () => {
    const { place, asked } = open(`/finance/runs/${failed}`);
    await screen.findByText("invoice · shard 3/8 · attempt 2");
    expect(line()).toEqual(["↑↓ Step", "[] Port", "p Replay from invoice", "esc All runs of finance", "1234 Runs, workflows, statistics, settings", "? Every key"]);
    await press("ArrowUp");
    expect(place.query.get("step")).toBe("normalize");
    await press("Escape");
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: undefined });
  });

  it("moves between the ports that hold an envelope", async () => {
    const { place } = open(`/finance/runs/${failed}`);
    await screen.findByText("invoice · shard 3/8 · attempt 2");
    await press("]");
    expect(place.query.get("port")).toBe("error");
    await press("[");
    expect(place.query.get("port")).toBe("out");
  });

  it("replays from the step chosen once y answers the question p asks, and from nothing else", async () => {
    const s = scenario("alice");
    const replay = "01JMZ9A2B3C4D5E6F7G8H9J0K1";
    s[`POST /api/v1/runs/${failed}/replay`] = { status: 202, body: { run: replay, state: "queued", commit: "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2c4e6b8d01", replay_of: failed, replay_from: "invoice" } };
    const { asked, place } = open(`/finance/runs/${failed}`, s);
    await screen.findByText("invoice · shard 3/8 · attempt 2");
    await press("p");
    expect(screen.getByText("Replay this run from invoice?")).toBeTruthy();
    expect(line().slice(0, 2)).toEqual(["y Replay from invoice", "nesc Keep it"]);
    await press("x");
    await press("n");
    expect(screen.queryByText(/A new run starts there/)).toBeNull();
    expect(asked.some((a) => a.endsWith("/replay"))).toBe(false);
    await press("p");
    await press("y");
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: replay }));
    expect(asked.filter((a) => a.endsWith("/replay"))).toHaveLength(1);
  });

  it("cancels a run still going once y answers the question c asks", async () => {
    const s = scenario("alice");
    (s[`GET /api/v1/runs/${failed}`]!.body as { state: string }).state = "running";
    s[`POST /api/v1/runs/${failed}/cancel`] = { status: 202, body: { run: failed } };
    const { asked } = open(`/finance/runs/${failed}`, s);
    await screen.findByText("invoice · shard 3/8 · attempt 2");
    expect(line()).toContain("c Cancel run");
    await press("c");
    expect(screen.getByText("Cancel this run?")).toBeTruthy();
    await press("Escape");
    expect(screen.queryByText(/Its tasks in flight are stopped/)).toBeNull();
    await press("c");
    await press("y");
    expect(await screen.findByText("Cancelling.")).toBeTruthy();
    expect(asked.filter((a) => a === `POST /api/v1/runs/${failed}/cancel`)).toHaveLength(1);
  });

  it("offers neither key to a principal without workflow:run, and the key does nothing", async () => {
    const s = scenario("alice");
    const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
    me.permissions["finance/monthly-invoicing"] = ["workflow:read", "run:read", "run:read_data"];
    open(`/finance/runs/${failed}`, s);
    await screen.findByText("invoice · shard 3/8 · attempt 2");
    expect(line().some((l) => /Replay|Cancel/.test(l))).toBe(false);
    await press("p");
    expect(screen.queryByText(/A new run starts there/)).toBeNull();
  });
});

describe("the run diff's keys", () => {
  it("goes back to the first run with esc", async () => {
    const good = "01JMZ8Q6F1T7QK2N4D6F8H0A2F";
    const { place } = open(`/finance/runs/${failed}/against/${good}`);
    await screen.findAllByText(good);
    expect(line()[0]).toBe("esc Back to the first run");
    await press("Escape");
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: failed, against: undefined });
  });
});

describe("moving a selection", () => {
  it("starts at either end, moves one at a time and stops at the ends", () => {
    const items = ["a", "b", "c"];
    expect(moved(items, undefined, "j")).toBe("a");
    expect(moved(items, undefined, "ArrowUp")).toBe("c");
    expect(moved(items, "a", "ArrowDown")).toBe("b");
    expect(moved(items, "c", "j")).toBe("c");
    expect(moved(items, "a", "k")).toBe("a");
    expect(moved([], undefined, "j")).toBeUndefined();
  });

  it("writes a key as agk console does", () => {
    expect(["ArrowUp", "Enter", "Escape", "?", "j"].map(shown)).toEqual(["↑", "enter", "esc", "?", "j"]);
  });
});

describe("the workflow page's keys", () => {
  it("move between the steps as the graph draws them, and go back to the workflow's runs", async () => {
    const { place } = open("/finance/workflows/monthly-invoicing");
    await screen.findByRole("button", { name: /^Step normalize/ });
    expect(line()).toEqual(["↑↓ Step", "esc Runs of monthly-invoicing", "1234 Runs, workflows, statistics, settings", "? Every key"]);
    await press("j");
    expect(place.query.get("step")).toBe("invoice");
    await press("ArrowDown");
    expect(place.query.get("step")).toBe("archive");
    await press("k");
    expect(place.query.get("step")).toBe("invoice");
    await press("Escape");
    expect(place.route).toMatchObject({ kind: "namespace", namespace: "finance", view: "runs" });
    expect(place.query.get("workflow")).toBe("monthly-invoicing");
  });
});

describe("the runners' keys", () => {
  it("narrow the runners to one pool after another, and to every pool again", async () => {
    const { place } = open("/runners", scenario("dana"));
    await screen.findByRole("button", { name: "dmz" });
    expect(line()).toEqual(["↑↓ Pool", "? Every key"]);
    await press("j");
    expect(place.query.get("pool")).toBe("default");
    await press("j");
    expect(place.query.get("pool")).toBe("dmz");
    expect(line()).toEqual(["↑↓ Pool", "esc Every pool", "? Every key"]);
    await press("Escape");
    expect(place.query.get("pool")).toBeNull();
  });
});
