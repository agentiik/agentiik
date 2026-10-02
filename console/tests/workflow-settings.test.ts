import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario, type Scenario } from "./scenario";

// An installation answering from a scenario, keeping each request that writes with its body.
function open(path: string, s: Scenario = scenario("alice")) {
  const asked: { key: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    if (request.method !== "GET") {
      const text = await request.text();
      asked.push({ key, body: text ? JSON.parse(text) : undefined });
    }
    const recorded = s[`${key}${url.search}`] ?? s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

const permissions = (s: Scenario) => (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions;

describe("a workflow's settings", () => {
  it("are a tab of the workflow's page to a caller who may change its name, and offer that alone where it is all they hold", async () => {
    const { place } = open("/finance/workflows/monthly-invoicing");
    const nav = await screen.findByRole("navigation", { name: "monthly-invoicing, what is shown" });
    const tab = within(nav).getByRole("link", { name: "Settings" });
    await fireEvent.click(tab);
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing", tab: "settings" });
    const general = await screen.findByRole("region", { name: "General" });
    expect(within(general).getByRole("form", { name: "Rename the workflow" })).toBeTruthy();
    expect(within(general).getByText("https://agentiik.example.com/finance/monthly-invoicing.git")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Default branch" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Move" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Delete workflow" })).toBeNull();
  });

  it("are no tab, and nothing to open, to a caller who may change none of them", async () => {
    open("/team-ops/workflows/nightly");
    const nav = await screen.findByRole("navigation", { name: "nightly, what is shown" });
    expect(within(nav).getByRole("link", { name: "Files" })).toBeTruthy();
    expect(within(nav).queryByRole("link", { name: "Settings" })).toBeNull();
    cleanup();
    open("/team-ops/workflows/nightly/settings");
    expect((await screen.findAllByText("This page does not exist, or is not shared with you.")).length).toBeGreaterThan(0);
  });

  it("rename the workflow, sending the name alone, and open its settings under the new one", async () => {
    const s = scenario("alice");
    const repository = (s["GET /api/v1/finance/workflows/monthly-invoicing"]!.body as { repository: Record<string, unknown> }).repository;
    s["PATCH /api/v1/finance/workflows/monthly-invoicing"] = { status: 200, body: { ...repository, name: "invoicing" } };
    const { asked, place } = open("/finance/workflows/monthly-invoicing/settings", s);
    const form = await screen.findByRole("form", { name: "Rename the workflow" });
    const rename = within(form).getByRole("button", { name: "Rename" }) as HTMLButtonElement;
    expect(rename.disabled).toBe(true);
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "invoicing" } });
    await fireEvent.submit(form);
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "invoicing", tab: "settings" }));
    expect(asked).toEqual([{ key: "PATCH /api/v1/finance/workflows/monthly-invoicing", body: { name: "invoicing" } }]);
  });

  it("name a workflow's default branch and protect it, sending only what changed", async () => {
    const s = scenario("alice");
    const repository = (s["GET /api/v1/alice/workflows/report"]!.body as { repository: Record<string, unknown> }).repository;
    s["PATCH /api/v1/alice/workflows/report"] = { status: 200, body: { ...repository, protected: true } };
    const { asked } = open("/alice/workflows/report/settings", s);
    const form = await screen.findByRole("form", { name: "Default branch of alice/report" });
    expect((within(form).getByRole("textbox") as HTMLInputElement).value).toBe("main");
    const write = within(form).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(write.disabled).toBe(true);
    await fireEvent.click(within(form).getByRole("checkbox"));
    await fireEvent.submit(form);
    expect(await screen.findByText("Saved.")).toBeTruthy();
    expect(asked).toEqual([{ key: "PATCH /api/v1/alice/workflows/report", body: { protected: true } }]);
  });

  it("say what the API refused of a default branch as it said it", async () => {
    const s = scenario("alice");
    s["PATCH /api/v1/alice/workflows/report"] = { status: 422, body: { error: "the repository holds no branch release" } };
    open("/alice/workflows/report/settings", s);
    const form = await screen.findByRole("form", { name: "Default branch of alice/report" });
    await fireEvent.input(within(form).getByRole("textbox"), { target: { value: "release" } });
    await fireEvent.submit(form);
    expect(await screen.findByText(/^The repository holds no branch release\.$/)).toBeTruthy();
  });

  it("move the workflow to another namespace its caller owns, and link to it there", async () => {
    const s = scenario("alice");
    permissions(s)["finance"]!.push("grant:manage");
    s["PATCH /api/v1/alice/workflows/report"] = { status: 202 };
    const { asked } = open("/alice/workflows/report/settings", s);
    const pane = await screen.findByRole("region", { name: "Move" });
    const form = within(pane).getByRole("form", { name: "Move the workflow" });
    expect(within(form).getAllByRole("option").map((o) => o.textContent)).toEqual(["To the namespace", "finance"]);
    await fireEvent.change(within(form).getByRole("combobox", { name: "To the namespace" }), { target: { value: "finance" } });
    await fireEvent.submit(form);
    const there = await within(pane).findByRole("link", { name: "Open it there" });
    expect(there.getAttribute("href")).toBe("/finance/workflows/report");
    expect(asked).toEqual([{ key: "PATCH /api/v1/alice/workflows/report", body: { namespace: "finance" } }]);
  });

  it("delete the workflow once its name is typed again, then open the namespace's workflows", async () => {
    const s = scenario("alice");
    s["DELETE /api/v1/alice/workflows/report"] = { status: 202 };
    const { asked, place } = open("/alice/workflows/report/settings", s);
    const form = await screen.findByRole("form", { name: "Delete the workflow" });
    const remove = within(form).getByRole("button", { name: "Delete" }) as HTMLButtonElement;
    expect(remove.disabled).toBe(true);
    await fireEvent.input(within(form).getByRole("textbox", { name: "The workflow's name, typed again" }), { target: { value: "repor" } });
    expect(remove.disabled).toBe(true);
    await fireEvent.input(within(form).getByRole("textbox", { name: "The workflow's name, typed again" }), { target: { value: "report" } });
    expect(remove.disabled).toBe(false);
    await fireEvent.submit(form);
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "alice", view: "workflows" }));
    expect(asked).toEqual([{ key: "DELETE /api/v1/alice/workflows/report", body: undefined }]);
  });
});
