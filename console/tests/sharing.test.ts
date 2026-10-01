import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { carries, expiryOf, kindOf, principalsOf, resolve, type Grant } from "../src/lib/sharing";
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
  const [pathname, search] = path.split("?");
  const place = new Place({ pathname: pathname!, search: search ? `?${search}` : "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

// The documentation's own example: team-finance edits every workflow of finance, and a deny takes
// run:read_data from alice on monthly-invoicing alone.
const documented: Grant[] = [
  { id: "01M2AC5K0N2Q4S6T8W0Y2A4C6E", principal: "group:team-finance", scope: "finance", role: "editor", granted_by: "alice", granted_at: "2026-09-27T09:00:00Z" },
  { id: "01M2AC9Q4S6V8X0Z2B4D6F8H0K", principal: "alice", scope: "finance/monthly-invoicing", deny: "run:read_data", granted_by: "alice", granted_at: "2026-09-27T09:01:00Z" },
];
const now = Date.parse("2026-10-01T00:00:00Z");

function sum(lines: ReturnType<typeof resolve>) {
  return Object.fromEntries(lines.map((l) => [l.permission, l.takes.length > 0 ? "-" : l.held ? "+" : l.ifMember.length > 0 ? "?" : ""]));
}

describe("resolving a principal's permissions", () => {
  it("adds every applying grant up and takes each deny away, as the documentation's example does", () => {
    const alice = { ref: "alice", groups: ["team-finance"] };
    expect(sum(resolve(alice, documented, { namespace: "finance", workflow: "monthly-invoicing" }, now))).toEqual({
      "workflow:read": "+",
      "workflow:run": "+",
      "workflow:write": "+",
      "workflow:delete": "",
      "run:read": "+",
      "run:read_data": "-",
      "secret:use": "+",
      "secret:write": "+",
      "grant:manage": "",
    });
    // The deny is the workflow's alone: the namespace keeps run:read_data, and another member keeps it there.
    expect(sum(resolve(alice, documented, { namespace: "finance" }, now))["run:read_data"]).toBe("+");
    expect(sum(resolve({ ref: "bob", groups: ["team-finance"] }, documented, { namespace: "finance", workflow: "monthly-invoicing" }, now))["run:read_data"]).toBe("+");
  });

  it("names the grant each permission comes from and the deny that takes it", () => {
    const lines = resolve({ ref: "alice", groups: ["team-finance"] }, documented, { namespace: "finance", workflow: "monthly-invoicing" }, now);
    const data = lines.find((l) => l.permission === "run:read_data")!;
    expect(data.gives.map((g) => g.principal)).toEqual(["group:team-finance"]);
    expect(data.takes.map((g) => g.scope)).toEqual(["finance/monthly-invoicing"]);
  });

  it("never gives secret:use or secret:write through a grant on one workflow", () => {
    const grants: Grant[] = [{ id: "01M2AC5K0N2Q4S6T8W0Y2A4C6F", principal: "carol", scope: "finance/monthly-invoicing", role: "owner", granted_by: "alice", granted_at: "2026-09-27T09:00:00Z" }];
    const held = sum(resolve({ ref: "carol", groups: [] }, grants, { namespace: "finance", workflow: "monthly-invoicing" }, now));
    expect(held["grant:manage"]).toBe("+");
    expect(held["secret:use"]).toBe("");
    expect(held["secret:write"]).toBe("");
  });

  it("passes over a grant that has ended, at the instant its expiry names", () => {
    const grants: Grant[] = [{ id: "01M2AC5K0N2Q4S6T8W0Y2A4C6G", principal: "carol", scope: "finance", role: "viewer", expires_at: "2026-10-01T00:00:00Z", granted_by: "alice", granted_at: "2026-09-27T09:00:00Z" }];
    expect(sum(resolve({ ref: "carol", groups: [] }, grants, { namespace: "finance" }, now))["workflow:read"]).toBe("");
    expect(sum(resolve({ ref: "carol", groups: [] }, grants, { namespace: "finance" }, now - 1))["workflow:read"]).toBe("+");
  });

  it("says a group's grant applies to a member where the principal's groups cannot be read", () => {
    const lines = resolve({ ref: "bob" }, documented, { namespace: "finance" }, now);
    expect(sum(lines)["workflow:write"]).toBe("?");
    expect(lines.every((l) => !l.held)).toBe(true);
  });

  it("reads what each role carries off the documentation's columns", () => {
    expect([...carries("viewer")].sort()).toEqual(["run:read", "workflow:read"]);
    expect([...carries("operator")].sort()).toEqual(["run:read", "workflow:run"]);
    expect(carries("editor").has("workflow:delete")).toBe(false);
    expect(carries("owner").size).toBe(9);
  });

  it("tells a user, a group and a service account apart, and lists the principals in that order", () => {
    expect(["alice", "group:team-finance", "finance/agentiik"].map(kindOf)).toEqual(["user", "group", "service account"]);
    expect(principalsOf([...documented, { ...documented[0]!, principal: "finance/agentiik" }, { ...documented[0]!, principal: "bob" }])).toEqual(["alice", "bob", "group:team-finance", "finance/agentiik"]);
  });

  it("reads an expiry as days, a date or an instant, and refuses one that has passed", () => {
    expect(expiryOf("", now)).toBeUndefined();
    expect(expiryOf("30d", now)).toBe("2026-10-31T00:00:00.000Z");
    expect(expiryOf("2027-01-01", now)).toBe("2027-01-01T00:00:00.000Z");
    expect(expiryOf("2026-09-01", now)).toBeInstanceOf(Error);
    expect(expiryOf("soon", now)).toBeInstanceOf(Error);
  });
});

describe("the sharing panel", () => {
  it("lists a namespace's grants as written, its principals by kind and each deny apart", async () => {
    open("/alice/sharing");
    const grants = await screen.findByRole("region", { name: "Grants" });
    await within(grants).findByText("team-finance");
    const rows = within(grants).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelector("td")!.textContent!.replace(/\s+/g, " ").trim())).toEqual(["user alice", "group team-finance", "user bruno", "user bruno", "service account alice/ci"]);
    expect(within(grants).getByText("deny secret:write", { selector: ".chip" }).closest("tr")!.classList.contains("deny")).toBe(true);
    expect(within(grants).getByText("2026-12-31")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Sharing" }).getAttribute("aria-current")).toBe("page");
  });

  it("resolves the caller's permissions with its groups, each with the grant that gives it", async () => {
    open("/alice/sharing");
    const lines = await screen.findByRole("list", { name: "Effective permissions of alice on alice" });
    const grant = within(lines).getByText("grant:manage").closest("li")!;
    expect(grant.classList.contains("held")).toBe(true);
    expect(grant.textContent).toContain("owner at alice");
    // alice is in team-finance, whose viewer grant gives her workflow:read beside her own.
    expect(within(lines).getByText("workflow:read").closest("li")!.textContent).toContain("viewer at alice, through group:team-finance");
  });

  it("shows a deny winning over the role that gave the permission", async () => {
    open("/alice/sharing");
    await screen.findByRole("list", { name: /Effective permissions/ });
    await fireEvent.change(screen.getByRole("combobox", { name: "Whom to resolve" }), { target: { value: "bruno" } });
    const lines = await screen.findByRole("list", { name: "Effective permissions of bruno on alice" });
    const secret = within(lines).getByText("secret:write").closest("li")!;
    expect(secret.classList.contains("taken")).toBe(true);
    expect(secret.textContent).toContain("denied: deny secret:write at alice, over editor at alice");
    // Whether bruno is in team-finance is an administrator's to read.
    expect(screen.getByText(/Who is in a group is an administrator's to read/)).toBeTruthy();
  });

  it("reads anybody's groups for an administrator, and counts them", async () => {
    const s = scenario("alice");
    (s["GET /api/v1/me"]!.body as { admin: boolean }).admin = true;
    s["GET /api/v1/groups"] = { status: 200, body: { groups: [{ kind: "group", name: "team-finance", members: ["alice", "bruno"] }] } };
    open("/alice/sharing", s);
    await screen.findByRole("list", { name: /Effective permissions/ });
    await fireEvent.change(screen.getByRole("combobox", { name: "Whom to resolve" }), { target: { value: "bruno" } });
    const lines = await screen.findByRole("list", { name: "Effective permissions of bruno on alice" });
    expect(within(lines).getByText("workflow:read").closest("li")!.textContent).toContain("through group:team-finance");
    expect(screen.queryByText(/Who is in a group is an administrator's to read/)).toBeNull();
  });

  it("grants a role with an expiry from the one control, and reads the grants again", async () => {
    const s = scenario("alice");
    s["POST /api/v1/alice/grants"] = { status: 201, body: { id: "01JMZ7C0K1M2N3P4Q5R6S7T8V9", principal: "carol", scope: "alice", role: "editor", expires_at: "2026-10-31T00:00:00Z", granted_by: "alice", granted_at: "2026-10-01T00:00:00Z" } };
    const { asked } = open("/alice/sharing", s);
    const form = await screen.findByRole("form", { name: "Add a grant or a deny" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Who" }), { target: { value: "carol" } });
    await fireEvent.change(within(form).getByRole("combobox", { name: "Gives" }), { target: { value: "role:editor" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Expires" }), { target: { value: "30d" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("carol holds editor on alice.")).toBeTruthy();
    const posted = asked.find((a) => a.key === "POST /api/v1/alice/grants")!.body as { principal: string; role: string; expires_at: string };
    expect(posted.principal).toBe("carol");
    expect(posted.role).toBe("editor");
    expect(Date.parse(posted.expires_at) - Date.now()).toBeGreaterThan(29 * 86_400_000);
    expect(asked.filter((a) => a.key === "GET /api/v1/alice/grants")).toHaveLength(2);
  });

  it("denies one permission to a group, and says what the API refused as it said it", async () => {
    const s = scenario("alice");
    s["POST /api/v1/alice/grants"] = { status: 409, body: { error: "this would leave no administrator able to sign in" } };
    const { asked } = open("/alice/sharing", s);
    const form = await screen.findByRole("form", { name: "Add a grant or a deny" });
    await fireEvent.change(within(form).getByRole("combobox", { name: "What the principal is" }), { target: { value: "group" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Who" }), { target: { value: "team-ops" } });
    await fireEvent.change(within(form).getByRole("combobox", { name: "Gives" }), { target: { value: "deny:run:read_data" } });
    expect(within(form).getByRole("button", { name: "Deny run:read_data" })).toBeTruthy();
    await fireEvent.submit(form);
    expect(await screen.findByText("This would leave no administrator able to sign in.")).toBeTruthy();
    expect(asked.find((a) => a.key === "POST /api/v1/alice/grants")!.body).toEqual({ principal: "group:team-ops", deny: "run:read_data" });
  });

  it("refuses an expiry it cannot read before anything is sent", async () => {
    const { asked } = open("/alice/sharing");
    const form = await screen.findByRole("form", { name: "Add a grant or a deny" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Who" }), { target: { value: "carol" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Expires" }), { target: { value: "soon" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("The expiry is not a number of days, a date or an instant.")).toBeTruthy();
    expect(asked.some((a) => a.key.startsWith("POST"))).toBe(false);
  });

  it("revokes a grant on a second click alone", async () => {
    const s = scenario("alice");
    s["DELETE /api/v1/alice/grants/01JMZ7A8F9G0H1J2K3M4N5P6Q7"] = { status: 204 };
    const { asked } = open("/alice/sharing", s);
    const grants = await screen.findByRole("region", { name: "Grants" });
    const row = (await within(grants).findByText("alice/ci")).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Revoke" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Revoke it" }));
    expect(await screen.findByText(/The operator grant to alice\/ci is revoked/)).toBeTruthy();
    expect(asked.filter((a) => a.key.startsWith("DELETE")).map((a) => a.key)).toEqual(["DELETE /api/v1/alice/grants/01JMZ7A8F9G0H1J2K3M4N5P6Q7"]);
  });

  it("opens a workflow's grants, those it inherits said to be and revoked only where they are written", async () => {
    const { place } = open("/alice/sharing");
    await screen.findByRole("region", { name: "Grants" });
    await fireEvent.input(screen.getByRole("textbox", { name: "A workflow of alice" }), { target: { value: "report" } });
    await fireEvent.submit(screen.getByRole("textbox", { name: "A workflow of alice" }).closest("form")!);
    expect(place.query.get("workflow")).toBe("report");
    const grants = await screen.findByRole("region", { name: "Grants" });
    await within(grants).findByText("7 on alice/report");
    const inherited = within(grants).getAllByText("inherited");
    expect(inherited).toHaveLength(5);
    expect(within(inherited[0]!.closest("tr")!).queryByRole("button", { name: "Revoke" })).toBeNull();
    await fireEvent.change(screen.getByRole("combobox", { name: "Whom to resolve" }), { target: { value: "bruno" } });
    const lines = await screen.findByRole("list", { name: "Effective permissions of bruno on alice/report" });
    expect(within(lines).getByText("run:read_data").closest("li")!.textContent).toContain("deny run:read_data at alice/report");
  });

  it("shows what each role carries, operator's lack of workflow:read marked", async () => {
    open("/alice/sharing");
    const pane = await screen.findByRole("region", { name: "Roles" });
    const operator = within(pane).getAllByRole("row").find((r) => r.querySelector("td")?.textContent === "operator")!;
    expect([...operator.querySelectorAll("td.mark")].map((c) => c.textContent)).toEqual(["no", "yes", "no", "no", "no", "no", "no"]);
    expect(operator.querySelector("td.lack")!.textContent).toBe("no");
    expect(within(pane).getByText(/runs a workflow and follows its runs/)).toBeTruthy();
  });

  it("says what sharing never exposes", async () => {
    open("/alice/sharing");
    const pane = await screen.findByRole("region", { name: "What sharing never exposes" });
    expect(within(pane).getAllByRole("listitem")).toHaveLength(4);
  });

  it("is offered nowhere the caller holds no grant:manage, and answered as what does not exist", async () => {
    open("/finance/runs");
    await screen.findAllByText("01JMZ8W4K2R7QX6T1N3P5V7Y9A");
    expect(screen.queryByRole("link", { name: "Sharing" })).toBeNull();
    open("/finance/sharing");
    await waitFor(() => expect(screen.getAllByText("No such thing, or not yours.").length).toBeGreaterThan(0));
  });
});
