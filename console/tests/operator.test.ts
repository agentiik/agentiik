import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

// An operator, who may run a workflow and follow its runs without reading it: alice's scenario with
// what she holds in finance cut down to the operator role, workflow:run and run:read, so that every
// route reading the workflow itself answers her as one that does not exist, as the API would.
function asOperator(): Scenario {
  const s = scenario("alice");
  const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
  me.permissions.finance = ["workflow:run", "run:read"];
  for (const key of Object.keys(s)) {
    if (/^GET \/api\/v1\/finance\/workflows\/[^/?]+(\?|$|\/tree|\/refs|\/triggers)/.test(key)) delete s[key];
  }
  return s;
}

function open(path: string, s: Scenario = asOperator(), sent: { path: string; body: unknown }[] = []) {
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

describe("an operator", () => {
  it("finds the workflows whose runs it follows under Workflows", async () => {
    open("/finance/workflows");
    const views = await screen.findByRole("list", { name: "Views of finance" });
    expect(within(views).getByRole("link", { name: "Workflows" })).toBeTruthy();
    expect((await screen.findByRole("link", { name: "monthly-invoicing" })).getAttribute("href")).toBe("/finance/workflows/monthly-invoicing");
  });

  it("opens a workflow on its runs, with neither its graph nor its files offered", async () => {
    open("/finance/workflows/monthly-invoicing");
    expect(await screen.findByRole("region", { name: "Runs of monthly-invoicing" })).toBeTruthy();
    const tabs = screen.getAllByRole("link").map((a) => a.textContent?.trim());
    expect(tabs).toContain("Runs");
    expect(tabs).toContain("Statistics");
    expect(tabs).not.toContain("Graph");
    expect(tabs).not.toContain("Files");
    expect(screen.queryByText("Not found")).toBeNull();
  });

  it("starts a run from there, asked for the inputs the route answers under workflow:run", async () => {
    const sent: { path: string; body: unknown }[] = [];
    const place = open("/finance/workflows/monthly-invoicing", asOperator(), sent);
    await fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    const dialog = await screen.findByRole("dialog", { name: "Run monthly-invoicing" });
    const orders = (await within(dialog).findByLabelText(/^orders/)) as HTMLTextAreaElement;
    // The schema a file of the tree holds is checked with the file the route answered beside it.
    const customers = within(dialog).getByLabelText(/^customers/) as HTMLTextAreaElement;
    await new Promise((r) => setTimeout(r, 0));
    await fireEvent.input(customers, { target: { value: '[{"vat": "FR"}]' } });
    await fireEvent.blur(customers);
    expect(await within(dialog).findByText('at /0: Instance does not have required property "id".')).toBeTruthy();
    await fireEvent.input(customers, { target: { value: '[{"id": "C-1"}]' } });
    await fireEvent.blur(customers);

    await fireEvent.input(orders, { target: { value: '[{"order": "ORD-0001", "amount": 12}]' } });
    await fireEvent.click(within(dialog).getByRole("button", { name: "Start the run" }));
    await waitFor(() => expect(sent).toEqual([{ path: "/api/v1/finance/workflows/monthly-invoicing/runs", body: { inputs: { orders: [{ order: "ORD-0001", amount: 12 }], customers: [{ id: "C-1" }] } } }]));
    await waitFor(() => expect(place.route).toMatchObject({ kind: "namespace", view: "workflows", run: "01JMZ9A2B3C4D5E6F7G8H9J0K1" }));
  });

  it("is answered Not found for a workflow's files, which are reading it", async () => {
    open("/finance/workflows/monthly-invoicing/files");
    expect(await screen.findByText("Not found")).toBeTruthy();
  });

  it("is offered Run only where it may ask for one", async () => {
    const s = asOperator();
    (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions.finance = ["run:read"];
    open("/finance/workflows/monthly-invoicing", s);
    expect(await screen.findByRole("region", { name: "Runs of monthly-invoicing" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run" })).toBeNull();
  });
});
