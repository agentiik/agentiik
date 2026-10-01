import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario, type Scenario } from "./scenario";

// An installation answering from a scenario, keeping each request with its body.
function installation(s: Scenario) {
  const asked: { key: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const text = request.method === "GET" ? "" : await request.text();
    asked.push({ key, body: text ? JSON.parse(text) : undefined });
    const recorded = s[`${key}${url.search}`] ?? s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
  return { asked, fetcher };
}

function open(path: string, s: Scenario = scenario("alice")) {
  const { asked, fetcher } = installation(s);
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

const declared = { name: "stripe-key", provider: "builtin", mount: "/agk/secrets/stripe-key", declared_by: "alice", declared_at: "2026-10-01T00:00:00Z" };

describe("a namespace's secrets", () => {
  it("lists where each value is kept and where a step is given it, and offers nothing to write without secret:write", async () => {
    open("/finance/settings");
    const pane = await screen.findByRole("region", { name: "Secrets" });
    const rows = await within(pane).findAllByRole("row");
    expect(rows.slice(1).map((r) => r.querySelector("td")!.textContent)).toEqual(["smtp-password", "stripe-key"]);
    expect(within(pane).getByText("AGK_DEV_FINANCE_SMTP_PASSWORD")).toBeTruthy();
    expect(within(pane).getByText("/agk/secrets/stripe-key")).toBeTruthy();
    expect(within(pane).getByText("under finance and its name")).toBeTruthy();
    expect(within(pane).queryByRole("button")).toBeNull();
    expect(screen.queryByRole("form")).toBeNull();
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
  });

  it("says why it shows none to a caller who does not read the namespace's workflows", async () => {
    const s = scenario("alice");
    (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions["finance"] = ["run:read"];
    const { asked } = open("/finance/settings", s);
    expect(await screen.findByText(/shown to whoever reads its workflows/)).toBeTruthy();
    expect(asked.some((a) => a.key.endsWith("/secrets"))).toBe(false);
  });

  it("declares a secret with its value written once, and shows the value nowhere", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/stripe-key"] = { status: 201, body: declared };
    const { asked } = open("/alice/settings", s);
    const form = await screen.findByRole("form", { name: "Declare a secret" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "stripe-key" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Value" }), { target: { value: "sk_live_never_shown" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("stripe-key is declared and its value written. It is shown nowhere.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/alice/secrets/stripe-key")!.body).toEqual({ provider: "builtin", value: "sk_live_never_shown" });
    expect(document.body.innerHTML).not.toContain("sk_live_never_shown");
    expect((within(form).getByRole("textbox", { name: "Value" }) as HTMLTextAreaElement).value).toBe("");
  });

  it("declares one kept in the API's environment by the variable it is read from", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/smtp"] = { status: 201, body: { ...declared, name: "smtp", provider: "env", path: "AGK_DEV_ALICE_SMTP", mount: "/agk/secrets/smtp" } };
    const { asked } = open("/alice/settings", s);
    const form = await screen.findByRole("form", { name: "Declare a secret" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "smtp" } });
    await fireEvent.change(within(form).getByRole("combobox", { name: "Kept in" }), { target: { value: "env" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Variable" }), { target: { value: "AGK_DEV_ALICE_SMTP" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("smtp is declared, kept in env.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/alice/secrets/smtp")!.body).toEqual({ provider: "env", path: "AGK_DEV_ALICE_SMTP" });
  });

  it("rotates a value by writing it again, as base64 where it is not text", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/github-token"] = { status: 200, body: { ...declared, name: "github-token", mount: "/agk/secrets/github-token" } };
    const { asked } = open("/alice/settings", s);
    const row = (await screen.findByText("github-token", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Rotate" }));
    const form = screen.getByRole("form", { name: "Rotate github-token" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Value" }), { target: { value: "Z2hwX25ldw==" } });
    await fireEvent.click(within(form).getByRole("checkbox"));
    await fireEvent.submit(form);
    expect(await screen.findByText(/The value of github-token is written/)).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/alice/secrets/github-token")!.body).toEqual({ provider: "builtin", value: "Z2hwX25ldw==", encoding: "base64" });
  });

  it("removes a declaration on a second click alone", async () => {
    const s = scenario("alice");
    s["DELETE /api/v1/alice/secrets/github-token"] = { status: 204 };
    const { asked } = open("/alice/settings", s);
    const row = (await screen.findByText("github-token", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Remove it" }));
    expect(await screen.findByText(/github-token is removed, its value with it/)).toBeTruthy();
    expect(asked.filter((a) => a.key.startsWith("DELETE")).map((a) => a.key)).toEqual(["DELETE /api/v1/alice/secrets/github-token"]);
  });

  it("says what the API refused as it said it", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/smtp"] = { status: 400, body: { error: "env is not a store this installation reads the secrets of alice from" } };
    open("/alice/settings", s);
    const form = await screen.findByRole("form", { name: "Declare a secret" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "smtp" } });
    await fireEvent.change(within(form).getByRole("combobox", { name: "Kept in" }), { target: { value: "env" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Variable" }), { target: { value: "AGK_DEV_ALICE_SMTP" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("Env is not a store this installation reads the secrets of alice from.")).toBeTruthy();
  });
});
