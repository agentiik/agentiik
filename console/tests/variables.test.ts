import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { shown, written } from "../src/lib/variables";
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

const stored = { name: "ledger_url", value: "https://ledger.example.com/api", visibility: "all", updated_by: "alice", updated_at: "2026-10-01T06:00:00Z" };

async function rows() {
  const pane = await screen.findByRole("region", { name: "Variables" });
  return (await within(pane).findAllByRole("row")).slice(1);
}

describe("a namespace's variables", () => {
  it("lists each by its name with its value and the workflows that read it", async () => {
    open("/finance/variables");
    const listed = await rows();
    expect(listed.map((r) => r.querySelector("td")!.textContent)).toEqual(["currency", "dunning_days", "invoice_footer", "ledger_url", "reminder_days"]);
    const cells = (name: string) => [...listed.find((r) => r.querySelector("td")!.textContent === name)!.querySelectorAll("td")].map((c) => c.textContent!.trim());
    expect(cells("currency").slice(1, 3)).toEqual(["EUR", "All workflows"]);
    expect(cells("dunning_days").slice(1, 3)).toEqual(["45", "None"]);
    expect(cells("reminder_days").slice(1, 3)).toEqual(["[7,14,30]", "monthly-invoicing, payment-reminders"]);
    expect(within(listed[4]!).getByRole("link", { name: "payment-reminders" }).getAttribute("href")).toBe("/finance/workflows/payment-reminders");
    expect(screen.getByRole("link", { name: "Variables" }).getAttribute("aria-current")).toBe("page");
  });

  it("sits in the sidebar between Sharing and Settings", async () => {
    open("/alice/variables");
    await screen.findByText("alice has no variable.");
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    const labels = within(nav).getAllByRole("link").map((a) => a.textContent!.trim());
    expect(labels.indexOf("Variables")).toBe(labels.indexOf("Sharing") + 1);
    expect(labels.indexOf("Settings")).toBe(labels.indexOf("Variables") + 1);
  });

  it("offers nothing to write without workflow:write", async () => {
    open("/team-ops/variables");
    const listed = await rows();
    expect(listed).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "New variable" })).toBeNull();
    expect(within(listed[0]!).queryByRole("button")).toBeNull();
  });

  it("is left out of the sidebar and reads nothing without workflow:read at the namespace's scope", async () => {
    const s = scenario("alice");
    (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions["finance"] = ["run:read"];
    const { asked } = open("/finance/variables", s);
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Variables" })).toBeNull();
    expect(asked.some((a) => a.key.endsWith("/variables"))).toBe(false);
  });

  it("creates one for every workflow, its value the text typed", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/finance/variables/ledger_url"] = { status: 201, body: stored };
    const { asked } = open("/finance/variables", s);
    await fireEvent.click(await screen.findByRole("button", { name: "New variable" }));
    const form = screen.getByRole("form", { name: "New variable" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "ledger_url" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Value" }), { target: { value: "https://ledger.example.com/api" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("ledger_url saved.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/finance/variables/ledger_url")!.body).toEqual({ value: "https://ledger.example.com/api", visibility: "all" });
    expect(screen.queryByRole("form", { name: "New variable" })).toBeNull();
  });

  it("edits one's value and workflows, sending the whole variable with a workflow typed before it is pushed", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/finance/variables/reminder_days"] = { status: 200, body: { ...stored, name: "reminder_days", value: "7, 14", visibility: "selected", workflows: ["dunning", "monthly-invoicing"] } };
    const { asked } = open("/finance/variables", s);
    const row = (await screen.findByText("reminder_days", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Edit" }));
    const form = screen.getByRole("form", { name: "Edit reminder_days" });
    expect((within(form).getByRole("textbox", { name: "Value" }) as HTMLTextAreaElement).value).toBe("[7,14,30]");
    expect((within(form).getByRole("radio", { name: "Selected workflows" }) as HTMLInputElement).checked).toBe(true);
    await fireEvent.input(within(form).getByRole("textbox", { name: "Value" }), { target: { value: "7, 14" } });
    await fireEvent.click(within(form).getByRole("checkbox", { name: "payment-reminders" }));
    await fireEvent.input(within(form).getByRole("textbox", { name: "Another workflow" }), { target: { value: "dunning" } });
    await fireEvent.click(within(form).getByRole("button", { name: "Add" }));
    expect((within(form).getByRole("checkbox", { name: "dunning" }) as HTMLInputElement).checked).toBe(true);
    await fireEvent.submit(form);
    expect(await screen.findByText("reminder_days updated.")).toBeTruthy();
    expect(asked.find((a) => a.key === "PUT /api/v1/finance/variables/reminder_days")!.body).toEqual({ value: "7, 14", visibility: "selected", workflows: ["monthly-invoicing", "dunning"] });
  });

  it("widens one to every workflow without sending the list it had", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/finance/variables/invoice_footer"] = { status: 200, body: { ...stored, name: "invoice_footer" } };
    const { asked } = open("/finance/variables", s);
    const row = (await screen.findByText("invoice_footer", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Edit" }));
    const form = screen.getByRole("form", { name: "Edit invoice_footer" });
    await fireEvent.click(within(form).getByRole("radio", { name: "All workflows" }));
    expect(within(form).queryByRole("checkbox")).toBeNull();
    await fireEvent.submit(form);
    expect(await screen.findByText("invoice_footer updated.")).toBeTruthy();
    const body = asked.find((a) => a.key === "PUT /api/v1/finance/variables/invoice_footer")!.body as Record<string, unknown>;
    expect(body.visibility).toBe("all");
    expect("workflows" in body).toBe(false);
  });

  it("removes one on a second click alone", async () => {
    const s = scenario("alice");
    s["DELETE /api/v1/finance/variables/currency"] = { status: 204 };
    const { asked } = open("/finance/variables", s);
    const row = (await screen.findByText("currency", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("currency removed.")).toBeTruthy();
    expect(asked.filter((a) => a.key.startsWith("DELETE")).map((a) => a.key)).toEqual(["DELETE /api/v1/finance/variables/currency"]);
  });

  it("says what the API refused as it said it", async () => {
    const s = scenario("alice");
    s["PUT /api/v1/finance/variables/big"] = { status: 413, body: { error: "the value is 70000 bytes written as compact JSON, past the 65536 a variable holds" } };
    open("/finance/variables", s);
    await fireEvent.click(await screen.findByRole("button", { name: "New variable" }));
    const form = screen.getByRole("form", { name: "New variable" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "big" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Value" }), { target: { value: "1" } });
    await fireEvent.submit(form);
    expect(await screen.findByText(/It is too large: the value is 70000 bytes written as compact JSON, past the 65536 a variable holds/)).toBeTruthy();
  });
});

describe("a variable's value", () => {
  it("is shown as its text, and one the API holds as anything else as one line of JSON", () => {
    expect(shown("EUR")).toBe("EUR");
    expect(shown('"quoted"')).toBe('"quoted"');
    expect(shown(30)).toBe("30");
    expect(shown(null)).toBe("null");
    expect(shown({ a: [1, 2] })).toBe('{"a":[1,2]}');
  });

  it("is written as the text typed, whatever it holds, empty included", () => {
    expect(written("EUR")).toBe("EUR");
    expect(written("30")).toBe("30");
    expect(written(" spaced ")).toBe(" spaced ");
    expect(written("")).toBe("");
    expect(written("[7,14]", { value: [7, 14, 30] })).toBe("[7,14]");
  });

  it("keeps a value that is not text as it is when its text is left unchanged", () => {
    expect(written("[7,14,30]", { value: [7, 14, 30] })).toEqual([7, 14, 30]);
    expect(written("45", { value: 45 })).toBe(45);
    expect(written("EUR", { value: "EUR" })).toBe("EUR");
  });
});
