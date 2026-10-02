import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario, type Scenario } from "./scenario";

// An installation answering from a scenario, keeping every request it was asked with its query.
function open(path: string, s: Scenario) {
  const asked: string[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}${url.search}`);
    const recorded = s[`${request.method} ${url.pathname}${url.search}`] ?? s[`${request.method} ${url.pathname}`] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
  const api = connect("http://stand-in/", fetcher);
  const [pathname, search] = path.split("?");
  const place = new Place({ pathname: pathname!, search: search ? `?${search}` : "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

const audits = (asked: string[]) => asked.filter((a) => a.startsWith("GET /api/v1/auth/audit"));

describe("the audit log", () => {
  it("is an entry of the administration, the newest entries first, each opening on its detail", async () => {
    open("/users/audit", scenario("dana"));
    const nav = await screen.findByRole("list", { name: "Administration" });
    expect(within(nav).getByRole("link", { name: "Audit log" }).getAttribute("aria-current")).toBe("page");
    expect(within(nav).getByRole("link", { name: "Users" }).getAttribute("aria-current")).toBeNull();
    const pane = await screen.findByRole("region", { name: "Audit log" });
    const rows = await within(pane).findAllByRole("row");
    expect(rows[1]!.textContent).toMatch(/^12.*dana.*runner\.drain.*installation.*unchanged$/);
    expect(within(pane).getByText("12").closest("td")!.classList.contains("unproved")).toBe(true);
    expect(within(pane).getByText("10").closest("td")!.classList.contains("unproved")).toBe(false);
    await fireEvent.click(within(pane).getByRole("button", { name: "11" }));
    expect(within(pane).getByText(/"reason": "wrong month"/)).toBeTruthy();
    expect(document.title).toBe("Audit log · Agentiik");
  });

  it("is narrowed as typed, the address keeping it, and read a page further with Older", async () => {
    const s = scenario("dana");
    const full = s["GET /api/v1/auth/audit"]!.body as { entries: { seq: number }[]; head: number; verified: number };
    const fifty = Array.from({ length: 50 }, (_, i) => ({ ...full.entries[0]!, seq: 200 - i }));
    s["GET /api/v1/auth/audit?limit=50"] = { status: 200, body: { ...full, entries: fifty, head: 200 } };
    s["GET /api/v1/auth/audit?limit=50&before=151"] = { status: 200, body: { ...full, head: 200 } };
    const { asked, place } = open("/users/audit", s);
    const older = await screen.findByRole("button", { name: "Older" });
    await fireEvent.click(older);
    await waitFor(() => expect(audits(asked)).toContain("GET /api/v1/auth/audit?limit=50&before=151"));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Older" })).toBeNull());

    const form = screen.getByRole("form", { name: "Narrow the audit log" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Actor" }), { target: { value: "alice" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Namespace" }), { target: { value: "-" } });
    await fireEvent.submit(form);
    expect(place.query.get("actor")).toBe("alice");
    expect(place.query.get("namespace")).toBe("-");
    await waitFor(() => expect(audits(asked).at(-1)).toBe("GET /api/v1/auth/audit?limit=50&actor=alice&namespace=-"));
    await fireEvent.click(within(form).getByRole("button", { name: "Clear" }));
    expect(place.query.get("actor")).toBeNull();
  });

  it("is no page of a caller who administers nothing", async () => {
    open("/users/audit", scenario("alice"));
    expect((await screen.findAllByText("This page does not exist, or is not shared with you.")).length).toBeGreaterThan(0);
  });
});
