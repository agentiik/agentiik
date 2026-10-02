import { fireEvent, render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { fieldOf, problems, read, validator } from "../src/lib/run-form";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

describe("a run form's fields", () => {
  it("are typed as each input's schema types it, anything else written as JSON", () => {
    expect(fieldOf("n", { schema: { type: "integer" }, required: true }).kind).toBe("integer");
    expect(fieldOf("e", { schema: { enum: ["per_line", "per_invoice"] }, default: "per_line" })).toMatchObject({ kind: "enum", hasDefault: true });
    expect(fieldOf("o", { schema: { type: "array" } }).kind).toBe("json");
    expect(fieldOf("b", { schema: true }).kind).toBe("json");
  });

  it("read what was typed as the run would be given it, an empty field left to its default", () => {
    const n = fieldOf("n", { schema: { type: "integer" }, required: true });
    expect(read(n, "3")).toEqual({ value: 3, empty: false });
    expect(read(n, "3.5").error).toBe("not a whole number");
    expect(read(n, "").error).toBe("required, and it declares no default");
    expect(read(fieldOf("d", { schema: { type: "string" }, required: true, default: "x" }), "")).toEqual({ empty: true });
    expect(read(fieldOf("j", { schema: { type: "object" } }), "{oops").error).toMatch(/^not JSON/);
  });

  it("are checked against their schema with the files of the tree it names, and say where a value fails", async () => {
    const files: Record<string, unknown> = {
      "schemas/customers.json": { type: "array", items: { $ref: "./customer.json" } },
      "schemas/customer.json": { type: "object", required: ["id"], properties: { id: { type: "string" } } },
    };
    const asked: string[] = [];
    const v = await validator({ $ref: "./schemas/customers.json" }, async (path) => {
      asked.push(path);
      return files[path];
    });
    expect(asked).toEqual(["schemas/customers.json", "schemas/customer.json"]);
    expect(problems(v!, [{ id: "C001" }])).toEqual([]);
    expect(problems(v!, [{ vat: "FR" }])).toEqual(['at /0: Instance does not have required property "id".']);
    expect(await validator({ $ref: "./missing.json" }, async () => undefined)).toBeUndefined();
  });
});

function open(path: string, s: Scenario = scenario("alice"), sent: { path: string; body: unknown }[] = []) {
  const answer = answering(s);
  const api = connect("http://stand-in/", async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === "POST") sent.push({ path: new URL(request.url).pathname, body: await request.clone().json().catch(() => null) });
    return answer(request);
  });
  const place = new Place({ pathname: path, search: "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

describe("the manual run form", () => {
  it("refuses a value its schema refuses before anything is sent, and starts the run once it holds", async () => {
    const sent: { path: string; body: unknown }[] = [];
    const place = open("/finance/workflows/monthly-invoicing", scenario("alice"), sent);
    await fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    const orders = (await screen.findByLabelText(/^orders/)) as HTMLTextAreaElement;
    await fireEvent.input(orders, { target: { value: '[{"order": "ORD-0001", "amount": "12"}]' } });
    await fireEvent.blur(orders);
    expect(await screen.findByText('at /0/amount: Instance type "string" is invalid. Expected "number".')).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Start the run" }));
    expect(await screen.findByText(/Some inputs are invalid\./)).toBeTruthy();
    expect(sent).toEqual([]);

    await fireEvent.input(orders, { target: { value: '[{"order": "ORD-0001", "amount": 12}]' } });
    await fireEvent.click(screen.getByRole("button", { name: "Start the run" }));
    await new Promise((r) => setTimeout(r, 0));
    expect(sent).toEqual([{ path: "/api/v1/finance/workflows/monthly-invoicing/runs", body: { inputs: { orders: [{ order: "ORD-0001", amount: 12 }], customers: [] } } }]);
    expect(place.route).toMatchObject({ kind: "namespace", view: "workflows", run: "01JMZ9A2B3C4D5E6F7G8H9J0K1" });
  });

  it("checks an input whose schema names files of the tree against them", async () => {
    open("/finance/workflows/monthly-invoicing");
    await fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    const customers = (await screen.findByLabelText(/^customers/)) as HTMLTextAreaElement;
    await new Promise((r) => setTimeout(r, 0));
    await fireEvent.input(customers, { target: { value: '[{"vat": "FR"}]' } });
    await fireEvent.blur(customers);
    expect(await screen.findByText('at /0: Instance does not have required property "id".')).toBeTruthy();
  });

  it("points at the input the API refuses, as it names it", async () => {
    const s = scenario("alice");
    s["POST /api/v1/finance/workflows/monthly-invoicing/runs"] = { status: 422, body: { error: "input orders: schema: /0/order: does not match pattern; run refused", input: "orders", rule: "schema" } };
    open("/finance/workflows/monthly-invoicing", s);
    await fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    await fireEvent.input(await screen.findByLabelText(/^orders/), { target: { value: '[{"order": "ORD-0001", "amount": 1}]' } });
    await fireEvent.click(screen.getByRole("button", { name: "Start the run" }));
    expect(await screen.findByText("input orders: schema: /0/order: does not match pattern; run refused")).toBeTruthy();
    expect(screen.getByText(/The input orders is invalid\./)).toBeTruthy();
  });

  it("is offered only to a caller holding workflow:run", async () => {
    open("/team-ops/workflows/nightly");
    expect(await screen.findByText("History")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run" })).toBeNull();
  });
});

describe("the MCP panel", () => {
  it("shows the tools as a client sees them, each input's schema as it stands", async () => {
    open("/finance/workflows/monthly-invoicing/mcp");
    expect(await screen.findByText(/^https?:\/\/[^/]+\/mcp\/finance\/monthly-invoicing$/)).toBeTruthy();
    expect(screen.getByText("create_invoices")).toBeTruthy();
    expect(screen.getByText("mode: sync · timeout: 2m")).toBeTruthy();
    expect(screen.getByText("idempotentHint: true · destructiveHint: false")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Tool create_invoices" }).textContent).toContain('"^ORD-[0-9]{4}$"');
  });

  it("is offered only where the version declares an mcp block", async () => {
    open("/team-ops/workflows/nightly");
    expect(await screen.findByText("History")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "MCP" })).toBeNull();
  });
});
