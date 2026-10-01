import { fireEvent, render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

// The console drawn against a recorded scenario, with no installation behind it.
function open(path: string, s: Scenario = scenario("alice")) {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(s, asked));
  const history = { pushState() {}, replaceState() {} };
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, history);
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

describe("the console", () => {
  it("opens on the caller's home, every namespace together, and stays there", async () => {
    const { place } = open("/");
    await screen.findByRole("button", { name: "You, alice" });
    expect(await screen.findByRole("region", { name: "Activity" })).toBeTruthy();
    expect(place.route).toEqual({ kind: "landing" });
  });

  it("lists a namespace's runs, and those that failed in the last hour apart", async () => {
    open("/finance/runs");
    expect(await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A")).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Runs" }).getAttribute("aria-current")).toBe("page");
    await fireEvent.click(screen.getByRole("button", { name: "You, alice" }));
    expect(screen.getByText("agentiik v0.6.0")).toBeTruthy();
  });

  it("answers a namespace the caller holds nothing in as one that does not exist", async () => {
    const { asked } = open("/payroll/runs");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
    expect(asked.some((a) => a.includes("namespace=payroll"))).toBe(false);
  });

  it("asks to sign in where the API knows no session", async () => {
    open("/finance/runs", { "GET /api/v1/me": { status: 401, body: { error: "sign in first" } } });
    expect(await screen.findByRole("button", { name: "Sign in with a passkey" })).toBeTruthy();
  });

  it("sends a session that may only enrol to the enrolment page", async () => {
    open("/finance/runs", { "GET /api/v1/me": { status: 403, body: { error: "enrol a passkey first" } } });
    expect((await screen.findByRole("link", { name: "Set up a passkey" })).getAttribute("href")).toBe("auth/enrol");
  });
});

describe("a run's steps and commit in the list", () => {
  it("draws each run's steps as a strip, said in words, and its pinned commit", async () => {
    open("/finance/runs");
    const strip = await screen.findByRole("img", { name: /^Steps: normalize succeeded in 50s, invoice failed in 3m 07s, archive not reached$/ });
    const segments = strip.querySelectorAll(".segment");
    expect(segments).toHaveLength(3);
    expect(Number((segments[1] as HTMLElement).style.flexGrow)).toBeGreaterThan(Number((segments[0] as HTMLElement).style.flexGrow));
    expect(segments[2]!.classList.contains("unreached")).toBe(true);
    const row = strip.closest("tr")!;
    const commit = row.querySelector(".commit")!;
    expect(commit.textContent).toHaveLength(7);
    expect(commit.getAttribute("title")!.startsWith(commit.textContent!)).toBe(true);
  });

  it("says a run that reached no step yet has none", async () => {
    open("/finance/runs");
    expect(await screen.findByRole("img", { name: "No step has been reached" })).toBeTruthy();
  });
});

describe("refusing as the API does", () => {
  // What is drawn in place of a page, with the address's own words taken out, so that two
  // addresses are compared on what the console says of them and not on what they spell.
  async function refusedAt(path: string, s: Scenario, named: string[]): Promise<string> {
    open(path, s);
    await screen.findByText("This page does not exist, or is not shared with you.");
    const drawn = document.body.innerHTML.replaceAll(/\s+/g, " ");
    document.body.innerHTML = "";
    return named.reduce((html, n) => html.replaceAll(n, "?"), drawn);
  }

  it("draws a run of a workflow the caller cannot read as a run that does not exist", async () => {
    const hidden = "01JMZ8Q0HIDDENHIDDENHIDDEN";
    const s = scenario("alice");
    s[`GET /api/v1/runs/${hidden}`] = { status: 404, body: { error: "no such thing, or not yours" } };
    const invisible = await refusedAt(`/finance/runs/${hidden}`, s, [hidden]);
    const absent = await refusedAt("/finance/runs/01JMZ8ZZZZZZZZZZZZZZZZZZZZ", scenario("alice"), ["01JMZ8ZZZZZZZZZZZZZZZZZZZZ"]);
    expect(invisible).toBe(absent);
  });

  it("draws a namespace the caller holds nothing in as one that does not exist", async () => {
    const invisible = await refusedAt("/payroll/runs", scenario("alice"), ["payroll"]);
    const absent = await refusedAt("/nowhere/runs", scenario("alice"), ["nowhere"]);
    expect(invisible).toBe(absent);
  });
});
