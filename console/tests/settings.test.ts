import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
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
    // A picture is sent as its bytes, which are kept as they came rather than read as JSON.
    const type = request.headers.get("Content-Type") ?? "";
    asked.push({ key, body: text ? (type.startsWith("image/") ? { type, bytes: text.length } : JSON.parse(text)) : undefined });
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
    expect(within(pane).getByText("stored by Agentiik")).toBeTruthy();
    expect(within(pane).queryByRole("button")).toBeNull();
    expect(screen.queryByRole("form")).toBeNull();
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
  });

  it("says why it shows none to a caller who does not read the namespace's workflows", async () => {
    const s = scenario("alice");
    (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions["finance"] = ["run:read"];
    const { asked } = open("/finance/settings", s);
    expect(await screen.findByText(/^Hidden/)).toBeTruthy();
    expect(asked.some((a) => a.key.endsWith("/secrets"))).toBe(false);
  });

  it("declares a secret with its value written once, and shows the value nowhere", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/stripe-key"] = { status: 201, body: declared };
    const { asked } = open("/alice/settings", s);
    await fireEvent.click(await screen.findByRole("button", { name: "New secret" }));
    const form = screen.getByRole("form", { name: "Declare a secret" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "stripe-key" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Value" }), { target: { value: "sk_live_never_shown" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("stripe-key saved.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/alice/secrets/stripe-key")!.body).toEqual({ provider: "builtin", value: "sk_live_never_shown" });
    expect(document.body.innerHTML).not.toContain("sk_live_never_shown");
    expect(screen.queryByRole("form", { name: "Declare a secret" })).toBeNull();
  });

  it("declares one kept in the API's environment by the variable it is read from", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/smtp"] = { status: 201, body: { ...declared, name: "smtp", provider: "env", path: "AGK_DEV_ALICE_SMTP", mount: "/agk/secrets/smtp" } };
    const { asked } = open("/alice/settings", s);
    await fireEvent.click(await screen.findByRole("button", { name: "New secret" }));
    const form = screen.getByRole("form", { name: "Declare a secret" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "smtp" } });
    await fireEvent.change(within(form).getByRole("combobox", { name: "Kept in" }), { target: { value: "env" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Variable" }), { target: { value: "AGK_DEV_ALICE_SMTP" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("smtp saved.")).toBeTruthy();
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
    expect(await screen.findByText("github-token updated.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/alice/secrets/github-token")!.body).toEqual({ provider: "builtin", value: "Z2hwX25ldw==", encoding: "base64" });
  });

  it("removes a declaration on a second click alone", async () => {
    const s = scenario("alice");
    s["DELETE /api/v1/alice/secrets/github-token"] = { status: 204 };
    const { asked } = open("/alice/settings", s);
    const row = (await screen.findByText("github-token", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("github-token removed.")).toBeTruthy();
    expect(asked.filter((a) => a.key.startsWith("DELETE")).map((a) => a.key)).toEqual(["DELETE /api/v1/alice/secrets/github-token"]);
  });

  it("says what the API refused as it said it", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/alice/secrets/smtp"] = { status: 400, body: { error: "env is not a store this installation reads the secrets of alice from" } };
    open("/alice/settings", s);
    await fireEvent.click(await screen.findByRole("button", { name: "New secret" }));
    const form = screen.getByRole("form", { name: "Declare a secret" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "smtp" } });
    await fireEvent.change(within(form).getByRole("combobox", { name: "Kept in" }), { target: { value: "env" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Variable" }), { target: { value: "AGK_DEV_ALICE_SMTP" } });
    await fireEvent.submit(form);
    expect(await screen.findByText(/Env is not a store this installation reads the secrets of alice from\./)).toBeTruthy();
  });
});

// alice's scenario with finance hers to manage, as its owner's, its former name and its picture set.
function owning(): Scenario {
  const s = scenario("alice");
  (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions["finance"]!.push("grant:manage");
  const listed = (s["GET /api/v1/namespaces"]!.body as { namespaces: Record<string, unknown>[] }).namespaces;
  Object.assign(listed.find((n) => n.name === "finance")!, { former_names: ["billing"], avatar_updated_at: "2026-09-30T08:00:00Z" });
  return s;
}

describe("a namespace's general settings", () => {
  it("show its name and picture, and offer nothing to change without grant:manage", async () => {
    open("/finance/settings");
    const pane = await screen.findByRole("region", { name: "General" });
    expect(within(pane).getByText("finance")).toBeTruthy();
    expect(within(pane).queryByRole("form", { name: "Rename the namespace" })).toBeNull();
    expect(within(pane).queryByRole("button")).toBeNull();
    expect(screen.queryByRole("region", { name: "Delete namespace" })).toBeNull();
    expect(screen.getByRole("region", { name: "Secrets" })).toBeTruthy();
  });

  it("rename a shared namespace its caller owns, list its former names and open it under the new one", async () => {
    const s = owning();
    s["PATCH /api/v1/namespaces/finance"] = { status: 200, body: { name: "accounting", kind: "shared", owner: "group:finance-leads", former_names: ["billing", "finance"], avatar_updated_at: null } };
    const { asked, place } = open("/finance/settings", s);
    const pane = await screen.findByRole("region", { name: "General" });
    expect(within(pane).getByText("billing")).toBeTruthy();
    const form = within(pane).getByRole("form", { name: "Rename the namespace" });
    const rename = within(form).getByRole("button", { name: "Rename" }) as HTMLButtonElement;
    expect(rename.disabled).toBe(true);
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "accounting" } });
    await fireEvent.submit(form);
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "accounting", view: "settings" }));
    expect(asked.find((a) => a.key === "PATCH /api/v1/namespaces/finance")!.body).toEqual({ name: "accounting" });
    expect(asked.filter((a) => a.key === "GET /api/v1/namespaces")).toHaveLength(2);
  });

  it("never offer a personal namespace a rename nor a removal, and give it a picture", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/namespaces/alice/avatar"] = { status: 204 };
    const { asked } = open("/alice/settings", s);
    const pane = await screen.findByRole("region", { name: "General" });
    expect(within(pane).queryByRole("form", { name: "Rename the namespace" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Delete namespace" })).toBeNull();
    expect(within(pane).queryByRole("button", { name: "Remove it" })).toBeNull();
    const file = new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], "mark.png", { type: "image/png" });
    await fireEvent.change(within(pane).getByLabelText("A picture, a PNG or a JPEG"), { target: { files: [file] } });
    expect(await screen.findByText("Picture saved.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/namespaces/alice/avatar")!.body).toEqual({ type: "image/png", bytes: 4 });
  });

  it("refuse a picture the API would refuse before sending it", async () => {
    const { asked } = open("/alice/settings");
    const pane = await screen.findByRole("region", { name: "General" });
    const file = new File(["GIF89a"], "mark.gif", { type: "image/gif" });
    await fireEvent.change(within(pane).getByLabelText("A picture, a PNG or a JPEG"), { target: { files: [file] } });
    expect(await screen.findByText("A picture must be a PNG or a JPEG file.")).toBeTruthy();
    expect(asked.some((a) => a.key.endsWith("/avatar"))).toBe(false);
  });

  it("draw the picture where the namespace is named, and remove it", async () => {
    const s = owning();
    s["DELETE /api/v1/namespaces/finance/avatar"] = { status: 204 };
    const { asked } = open("/finance/settings", s);
    const pane = await screen.findByRole("region", { name: "General" });
    const src = "api/v1/namespaces/finance/avatar?v=2026-09-30T08%3A00%3A00Z";
    expect(pane.querySelector("img")!.getAttribute("src")).toBe(src);
    expect(screen.getByRole("button", { name: "Switch namespace" }).querySelector("img")!.getAttribute("src")).toBe(src);
    await fireEvent.click(within(pane).getByRole("button", { name: "Remove it" }));
    expect(await screen.findByText("Picture removed.")).toBeTruthy();
    expect(asked.some((a) => a.key === "DELETE /api/v1/namespaces/finance/avatar")).toBe(true);
  });
});

describe("removing a namespace", () => {
  it("says what the API refused as it said it, what the namespace still holds", async () => {
    const s = owning();
    s["DELETE /api/v1/namespaces/finance"] = { status: 409, body: { error: "finance still holds 2 workflows, monthly-invoicing and payment-reminders: remove or move them first" } };
    const { asked } = open("/finance/settings", s);
    const pane = await screen.findByRole("region", { name: "Delete namespace" });
    await fireEvent.click(within(pane).getByRole("button", { name: "Delete" }));
    expect(asked.some((a) => a.key === "DELETE /api/v1/namespaces/finance")).toBe(false);
    await fireEvent.click(within(pane).getByRole("button", { name: "Delete" }));
    expect(await screen.findByText(/Finance still holds 2 workflows, monthly-invoicing and payment-reminders: remove or move them first\./)).toBeTruthy();
  });

  it("goes home once it is gone", async () => {
    const s = owning();
    s["DELETE /api/v1/namespaces/finance"] = { status: 204 };
    const { place } = open("/finance/settings", s);
    const pane = await screen.findByRole("region", { name: "Delete namespace" });
    await fireEvent.click(within(pane).getByRole("button", { name: "Delete" }));
    await fireEvent.click(within(pane).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(place.route).toEqual({ kind: "landing" }));
  });
});

describe("a new namespace", () => {
  it("is created from the switcher's foot, owned by its creator, and opened", async () => {
    const s = scenario("alice");
    s["POST /api/v1/namespaces"] = { status: 201, body: { name: "accounting", kind: "shared", owner: "alice", former_names: [], avatar_updated_at: null } };
    const { asked, place } = open("/finance/settings", s);
    await fireEvent.click(await screen.findByRole("button", { name: "Switch namespace" }));
    await fireEvent.click(screen.getByRole("button", { name: "New namespace" }));
    const form = screen.getByRole("form", { name: "New namespace" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "accounting" } });
    await fireEvent.submit(form);
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "accounting", view: "workflows" }));
    expect(asked.find((a) => a.key === "POST /api/v1/namespaces")!.body).toEqual({ name: "accounting" });
  });

  it("says why it was refused, in the dialog", async () => {
    const s = scenario("alice");
    s["POST /api/v1/namespaces"] = { status: 409, body: { error: "finance is already a namespace" } };
    open("/alice/settings", s);
    await fireEvent.click(await screen.findByRole("button", { name: "Switch namespace" }));
    await fireEvent.click(screen.getByRole("button", { name: "New namespace" }));
    const form = screen.getByRole("form", { name: "New namespace" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "finance" } });
    await fireEvent.submit(form);
    expect(await screen.findByText(/Finance is already a namespace\./)).toBeTruthy();
    expect(screen.getByRole("form", { name: "New namespace" })).toBeTruthy();
  });
});
