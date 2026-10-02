import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { beforeEach, describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { chosenLast, choose, matched, narrowed, opening, type Entry } from "../src/lib/palette";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

function open(path: string, s: Scenario = scenario("alice"), asked: string[] = []) {
  const answer = answering(s);
  const api = connect("http://stand-in/", async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}`);
    return answer(request);
  });
  const place = new Place({ pathname: path, search: "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

async function palette() {
  return screen.findByRole("dialog", { name: "Search" });
}

const shown = (dialog: HTMLElement) => within(dialog).queryAllByRole("option").map((o) => o.querySelector(".label")?.textContent?.trim());

beforeEach(() => {
  localStorage.clear();
});

describe("the palette's matching", () => {
  it("scores a word found whole above one whose letters are scattered, and more where it starts a word", () => {
    expect(matched("inv", "monthly-invoicing").score).toBe(153);
    expect(matched("oic", "monthly-invoicing").score).toBe(103);
    expect(matched("mi", "monthly-invoicing")).toEqual({ score: 2, at: [0, 8] });
    expect(matched("zz", "monthly-invoicing").score).toBe(-1);
  });

  it("keeps the entries every word matches, the closest first, a workflow found by its namespace too", () => {
    const entries: Entry[] = [
      { id: "workflow:finance/monthly-invoicing", kind: "workflow", label: "monthly-invoicing", detail: "finance" },
      { id: "workflow:team-ops/nightly", kind: "workflow", label: "nightly", detail: "team-ops" },
      { id: "view:workflows", kind: "view", label: "Workflows" },
    ];
    expect(narrowed(entries, "inv").map((f) => f.entry.id)).toEqual(["workflow:finance/monthly-invoicing"]);
    expect(narrowed(entries, "team night").map((f) => f.entry.id)).toEqual(["workflow:team-ops/nightly"]);
    expect(narrowed(entries, "o").map((f) => f.entry.id)).toEqual(["workflow:finance/monthly-invoicing", "view:workflows", "workflow:team-ops/nightly"]);
  });

  it("finds a run by the start of its identifier, never by letters inside it", () => {
    const run: Entry = { id: "run:01JMZ8W4", kind: "run", label: "01JMZ8W4K2R7QX6T1N3P5V7Y9A", detail: "finance/monthly-invoicing" };
    expect(narrowed([run], "01jmz8w")).toHaveLength(1);
    expect(narrowed([run], "K2R7")).toHaveLength(0);
    expect(narrowed([run], "invoicing")).toHaveLength(1);
  });

  it("keeps the five chosen last, the latest first, and opens on those still listed", () => {
    for (const id of ["a", "b", "c", "d", "e", "f", "b"]) choose(localStorage, id);
    expect(chosenLast(localStorage)).toEqual(["b", "f", "e", "d", "c"]);
    const entries: Entry[] = ["a", "c", "f", "z"].map((id) => ({ id, kind: "view", label: id }));
    const { recent, rest } = opening(entries, chosenLast(localStorage));
    expect(recent.map((e) => e.id)).toEqual(["f", "c"]);
    expect(rest.map((e) => e.id)).toEqual(["a", "z"]);
  });
});

describe("the palette", () => {
  it("opens on : and on Search in the sidebar, and closes on esc", async () => {
    open("/finance/workflows");
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    expect(document.activeElement).toBe(within(dialog).getByRole("combobox", { name: "Search" }));
    await fireEvent.keyDown(document.activeElement!, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull());
    await fireEvent.click(screen.getByRole("button", { name: /^Search/ }));
    expect(await palette()).toBeTruthy();
  });

  it("lists the views, the namespaces, the workflows and the latest runs the caller reads, and the keys of the screen", async () => {
    const asked: string[] = [];
    open("/finance/workflows", scenario("alice"), asked);
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    await within(dialog).findByText("dunning-reminders");
    const labels = shown(dialog);
    expect(labels).toEqual(expect.arrayContaining(["Home", "Workflows", "Statistics", "Your account", "finance", "team-ops", "monthly-invoicing", "01JMZ8W4K2R7QX6T1N3P5V7Y9A", "Every key"]));
    expect(labels).not.toContain("Search");
    expect(asked).toEqual(expect.arrayContaining(["GET /api/v1/finance/workflows", "GET /api/v1/team-ops/workflows", "GET /api/v1/runs"]));
  });

  it("narrows as letters are typed and opens the one chosen with enter", async () => {
    const place = open("/finance/workflows");
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    await within(dialog).findByText("dunning-reminders");
    const field = within(dialog).getByRole("combobox", { name: "Search" });
    await fireEvent.input(field, { target: { value: "dunning" } });
    expect(shown(dialog)[0]).toBe("dunning-reminders");
    await fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(place.route).toMatchObject({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "dunning-reminders" }));
    expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  });

  it("moves with the arrows and says nothing matches where nothing does", async () => {
    open("/finance/workflows");
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    const field = within(dialog).getByRole("combobox", { name: "Search" });
    await within(dialog).findByText("dunning-reminders");
    await fireEvent.input(field, { target: { value: "fin" } });
    const options = () => within(dialog).getAllByRole("option");
    expect(options()[0]!.getAttribute("aria-selected")).toBe("true");
    await fireEvent.keyDown(field, { key: "ArrowDown" });
    expect(options()[1]!.getAttribute("aria-selected")).toBe("true");
    expect(field.getAttribute("aria-activedescendant")).toBe(options()[1]!.id);
    await fireEvent.input(field, { target: { value: "qqqq" } });
    expect(within(dialog).getByText("Nothing matches.")).toBeTruthy();
  });

  it("opens on what was chosen last", async () => {
    choose(localStorage, "namespace:team-ops");
    open("/finance/workflows");
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    expect(within(dialog).getByText("Chosen last")).toBeTruthy();
    expect(shown(dialog)[0]).toBe("team-ops");
  });

  it("opens a run named whole however old, read on its own", async () => {
    const s = scenario("alice");
    const listing = s["GET /api/v1/runs"]!.body as { runs: { run: string }[] };
    const old = "01JMZ8V1P9C4XQ7K2N4D6F8H0A";
    listing.runs = listing.runs.filter((r) => r.run !== old);
    const place = open("/finance/workflows", s);
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    const field = within(dialog).getByRole("combobox", { name: "Search" });
    await fireEvent.input(field, { target: { value: old.toLowerCase() } });
    await waitFor(() => expect(shown(dialog)).toEqual([old]));
    await fireEvent.click(within(dialog).getByRole("option"));
    await waitFor(() => expect(place.route).toMatchObject({ kind: "namespace", namespace: "finance", workflow: "monthly-invoicing", run: old }));
  });

  it("does a key of the screen as the key would", async () => {
    open("/finance/workflows");
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    const field = within(dialog).getByRole("combobox", { name: "Search" });
    await fireEvent.input(field, { target: { value: "every key" } });
    await fireEvent.keyDown(field, { key: "Enter" });
    expect(await screen.findByRole("dialog", { name: "Every key of this view" })).toBeTruthy();
  });

  it("lists no administration to whoever does not administer", async () => {
    const s = scenario("alice");
    (s["GET /api/v1/me"]!.body as { admin: boolean }).admin = false;
    open("/finance/workflows", s);
    await screen.findByRole("link", { name: "monthly-invoicing" });
    await fireEvent.keyDown(window, { key: ":" });
    const dialog = await palette();
    expect(shown(dialog)).not.toContain("Audit log");
    expect(shown(dialog)).not.toContain("Runners");
  });
});
