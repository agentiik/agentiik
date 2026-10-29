import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario } from "./scenario";

// What the caller gives an MCP client, under their account: the user's server and their collections,
// each with the address a client is given and what each member offers; and from a workflow's MCP
// tab, the collections holding it, and adding it to another.

type Answer = { status: number; body?: unknown };

function open(path: string, extra: Record<string, Answer | Answer[]> = {}) {
  const answers: Record<string, Answer | Answer[]> = { ...scenario("alice"), ...extra };
  const asked: { key: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const text = request.method === "GET" ? "" : await request.text();
    asked.push({ key, body: text ? JSON.parse(text) : undefined });
    const held = answers[key];
    const answer = Array.isArray(held) ? (held.length > 1 ? held.shift()! : held[0]!) : (held ?? { status: 404, body: { error: "no such thing, or not yours" } });
    return new Response(answer.body === undefined ? null : JSON.stringify(answer.body), { status: answer.status, headers: { "Content-Type": "application/json" } });
  };
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.7.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

const collections = (scenario("alice")["GET /api/v1/me/collections"]!.body as { collections: Record<string, unknown>[] }).collections;

describe("the account's MCP tab", () => {
  it("gives the user's server's address and every collection with its address and what each member offers", async () => {
    open("/me/mcp");
    expect(await screen.findByText("http://stand-in/mcp")).toBeTruthy();
    const office = within(await screen.findByRole("region", { name: "Collection back-office" }));
    expect(office.getByText("https://agentiik.example.com/mcp/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C")).toBeTruthy();
    expect(office.getByText("create_invoices")).toBeTruthy();
    expect(office.getByText("no_mcp_block")).toBeTruthy();
    expect(office.getByText("v2")).toBeTruthy();
    const desk = within(screen.getByRole("region", { name: "Collection support_desk" }));
    expect(desk.getByText("not_runnable")).toBeTruthy();
    expect(screen.getByRole("link", { name: "An API token" })).toBeTruthy();
  });

  it("says of two members whose tools go by one name that each offers nothing for it", async () => {
    const taken = { workflow: "finance/payroll", tool: null, reason: "tool_name_taken" };
    const office = { ...collections[0]!, members: [{ workflow: "finance/monthly-invoicing", tool: null, reason: "tool_name_taken" }, taken] };
    open("/me/mcp", { "GET /api/v1/me/collections": { status: 200, body: { collections: [office, ...collections.slice(1)] } } });
    const shown = within(await screen.findByRole("region", { name: "Collection back-office" }));
    expect(shown.getAllByText("tool_name_taken")).toHaveLength(2);
    expect(shown.queryByText("no_mcp_block")).toBeNull();
  });

  it("says MCP off where the installation serves none, as a state and not a sentence, and keeps the collections", async () => {
    const meta = document.createElement("meta");
    meta.name = "agentiik-mcp";
    meta.content = "off";
    document.head.append(meta);
    try {
      open("/me/mcp");
      const server = await screen.findByRole("region", { name: "Your server" });
      expect(within(server).getByText("MCP off")).toBeTruthy();
      expect(within(server).queryByText(/AGK_MCP|answer 404|all the same/)).toBeNull();
      expect(await screen.findByRole("region", { name: "Collection back-office" })).toBeTruthy();
    } finally {
      meta.remove();
    }
  });

  it("says nothing of MCP off where the installation serves MCP", async () => {
    open("/me/mcp");
    await screen.findByRole("region", { name: "Collection back-office" });
    expect(screen.queryByText("MCP off")).toBeNull();
  });

  it("makes a collection with its name and description", async () => {
    const made = { id: "01JR8V0000000000000000000A", name: "nightly", description: "The nightly reports.", url: "https://agentiik.example.com/mcp/collections/01JR8V0000000000000000000A", members: [] };
    const { asked } = open("/me/mcp", {
      "POST /api/v1/me/collections": { status: 201, body: made },
      "GET /api/v1/me/collections": [{ status: 200, body: { collections } }, { status: 200, body: { collections: [...collections, made] } }],
    });
    await fireEvent.click(await screen.findByRole("button", { name: "New collection" }));
    const form = within(await screen.findByRole("form", { name: "New collection" }));
    await fireEvent.input(form.getByLabelText("Name"), { target: { value: "nightly" } });
    await fireEvent.input(form.getByLabelText("Description"), { target: { value: "The nightly reports." } });
    await fireEvent.submit(screen.getByRole("form", { name: "New collection" }));
    expect(await screen.findByText("nightly made.")).toBeTruthy();
    expect(asked.find((a) => a.key === "POST /api/v1/me/collections")?.body).toEqual({ name: "nightly", description: "The nightly reports." });
    expect(await screen.findByRole("region", { name: "Collection nightly" })).toBeTruthy();
  });

  it("takes a workflow out on a second click", async () => {
    const office = collections[0]!;
    const { asked } = open("/me/mcp", {
      "DELETE /api/v1/me/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C/members/finance/reconcile": { status: 200, body: { ...office, members: (office.members as unknown[]).slice(0, 1) } },
    });
    const region = within(await screen.findByRole("region", { name: "Collection back-office" }));
    const row = region.getByText("finance/reconcile").closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Take out" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getAllByRole("button", { name: "Take out" }).at(-1)!);
    await waitFor(() => expect(asked.some((a) => a.key === "DELETE /api/v1/me/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C/members/finance/reconcile")).toBe(true));
  });
});

describe("a workflow's MCP tab", () => {
  it("names the collections holding it with what it offers there, and adds it to another", async () => {
    const desk = collections[1]!;
    const { asked } = open("/finance/workflows/monthly-invoicing/mcp", {
      "PUT /api/v1/me/collections/01JR8T5Y7A9C1E3G5J7M9P1R3T/members/finance/monthly-invoicing": {
        status: 200,
        body: { ...desk, members: [...(desk.members as unknown[]), { workflow: "finance/monthly-invoicing", tool: "create_invoices" }] },
      },
    });
    const yours = within(await screen.findByRole("region", { name: "Your collections" }));
    const office = (await yours.findByText("back-office")).closest("tr")!;
    expect(within(office).getByText("create_invoices")).toBeTruthy();
    expect(within(office).queryByRole("button", { name: "Add" })).toBeNull();
    const supportDesk = yours.getByText("support_desk").closest("tr")!;
    await fireEvent.click(within(supportDesk).getByRole("button", { name: "Add" }));
    await waitFor(() => expect(within(supportDesk).getByText("create_invoices")).toBeTruthy());
    expect(asked.find((a) => a.key.startsWith("PUT"))?.body).toEqual({});
  });

  it("draws the tool by its labels and values, an async one's outputSchema none, and MCP off as a state", async () => {
    const detail = structuredClone(scenario("alice")["GET /api/v1/finance/workflows/monthly-invoicing"]!) as { status: number; body: { graph: { mcp: Record<string, unknown> } } };
    detail.body.graph.mcp = { ...detail.body.graph.mcp, mode: "async" };
    const meta = document.createElement("meta");
    meta.name = "agentiik-mcp";
    meta.content = "off";
    document.head.append(meta);
    try {
      open("/finance/workflows/monthly-invoicing/mcp", { "GET /api/v1/finance/workflows/monthly-invoicing": detail });
      const tool = within(await screen.findByRole("region", { name: "Tool create_invoices" }));
      expect(tool.getByText("mode: async · timeout: 2m · output: invoices")).toBeTruthy();
      expect(tool.getByText("outputSchema")).toBeTruthy();
      expect(tool.getByText("none")).toBeTruthy();
      expect(screen.getByText("MCP off")).toBeTruthy();
      expect(screen.queryByText(/run\.get|since an async call|AGK_MCP|no client is offered/)).toBeNull();
    } finally {
      meta.remove();
    }
  });

  it("offers no Add to a principal without workflow:run, which adding a member takes", async () => {
    const me = structuredClone(scenario("alice")["GET /api/v1/me"]!) as { status: number; body: { permissions: Record<string, string[]> } };
    me.body.permissions["finance"] = me.body.permissions["finance"]!.filter((p) => p !== "workflow:run");
    open("/finance/workflows/monthly-invoicing/mcp", { "GET /api/v1/me": me });
    const yours = within(await screen.findByRole("region", { name: "Your collections" }));
    const supportDesk = (await yours.findByText("support_desk")).closest("tr")!;
    expect(within(supportDesk).queryByRole("button", { name: "Add" })).toBeNull();
    expect(yours.queryAllByRole("button", { name: "Add" })).toEqual([]);
  });

  it("is no tab of a version that publishes no tool, and its address is a page that does not exist", async () => {
    open("/team-ops/workflows/nightly/mcp");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
    const tabs = within(screen.getByRole("navigation", { name: "nightly, what is shown" }));
    expect(tabs.queryByRole("link", { name: "MCP" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Your collections" })).toBeNull();
  });

  it("is no tab of a version whose block lists the tools of before v0.7.0, which publishes none", async () => {
    const detail = structuredClone(scenario("alice")["GET /api/v1/finance/workflows/monthly-invoicing"]!) as { status: number; body: { graph: { mcp: unknown } } };
    detail.body.graph.mcp = { tools: [{ name: "create_invoices", description: "Invoices the orders given." }] };
    open("/finance/workflows/monthly-invoicing", { "GET /api/v1/finance/workflows/monthly-invoicing": detail });
    // The graph drawn is the version read, whose block the tabs were decided on.
    await screen.findByRole("button", { name: /^Step normalize/ });
    const tabs = within(screen.getByRole("navigation", { name: "monthly-invoicing, what is shown" }));
    expect(tabs.getByRole("link", { name: "Runs" })).toBeTruthy();
    expect(tabs.queryByRole("link", { name: "MCP" })).toBeNull();
  });

  it("says why Add was refused at the foot of the screen, leaving the collections where they were", async () => {
    open("/finance/workflows/monthly-invoicing/mcp", {
      "PUT /api/v1/me/collections/01JR8T5Y7A9C1E3G5J7M9P1R3T/members/finance/monthly-invoicing": { status: 409, body: { error: "support_desk already offers a tool named create_invoices" } },
    });
    const yours = within(await screen.findByRole("region", { name: "Your collections" }));
    await fireEvent.click(within((await yours.findByText("support_desk")).closest("tr")!).getByRole("button", { name: "Add" }));
    expect(await screen.findByText("support_desk already offers a tool named create_invoices.")).toBeTruthy();
    expect(screen.getByText("support_desk already offers a tool named create_invoices.").closest(".notice")).toBeTruthy();
    expect(yours.queryByRole("alert")).toBeNull();
  });
});
