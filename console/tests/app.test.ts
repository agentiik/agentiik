import { render, screen } from "@testing-library/svelte";
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
  it("opens on the caller's own namespace", async () => {
    const { place } = open("/");
    await screen.findByText("alice", { selector: ".login" });
    expect(place.route).toEqual({ kind: "namespace", namespace: "alice", view: "runs", run: undefined });
  });

  it("lists a namespace's runs, and those that failed in the last hour apart", async () => {
    open("/finance/runs");
    expect(await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A")).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Runs" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByText("agentiik v0.6.0")).toBeTruthy();
  });

  it("answers a namespace the caller holds nothing in as one that does not exist", async () => {
    const { asked } = open("/payroll/runs");
    expect(await screen.findByText("No such thing, or not yours.")).toBeTruthy();
    expect(asked.some((a) => a.includes("namespace=payroll"))).toBe(false);
  });

  it("asks to sign in where the API knows no session", async () => {
    open("/finance/runs", { "GET /api/v1/me": { status: 401, body: { error: "sign in first" } } });
    expect(await screen.findByText("This browser is not signed in to this installation.")).toBeTruthy();
  });

  it("sends a session that may only enrol to the enrolment page", async () => {
    open("/finance/runs", { "GET /api/v1/me": { status: 403, body: { error: "enrol a passkey first" } } });
    expect((await screen.findByRole("link", { name: "Enrol a passkey" })).getAttribute("href")).toBe("auth/enrol");
  });
});
