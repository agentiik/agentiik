import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { bodyOf, formOf, inUnits, summary } from "../src/lib/quotas";
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

function open(path: string, s: Scenario = scenario("dana")) {
  const { asked, fetcher } = installation(s);
  const api = connect("http://stand-in/", fetcher);
  const [pathname, search] = path.split("?");
  const place = new Place({ pathname: pathname!, search: search ? `?${search}` : "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

const sent = (asked: { key: string; body: unknown }[], key: string) => asked.filter((a) => a.key === key).map((a) => a.body);

describe("a namespace's quotas in the form", () => {
  it("read back a size in the largest unit that holds it whole", () => {
    expect(inUnits(536870912000)).toEqual({ amount: 500, unit: "GiB" });
    expect(inUnits(2 * 1024 ** 4)).toEqual({ amount: 2, unit: "TiB" });
    expect(inUnits(1500)).toEqual({ amount: 1, unit: "MiB" });
  });

  it("start from the one that always holds a value, and leave every other bound out where it is empty", () => {
    const f = formOf(undefined);
    expect(f.max_concurrent_tasks).toBe("20");
    expect(f.max_retention_days).toBe("");
    expect(bodyOf(f)).toEqual({ body: { max_concurrent_tasks: 20 } });
    const g = { ...f, max_retention_days: "365", max_artifact: "500", max_artifact_unit: "GiB" as const, max_run_duration: "24h", allowed_runner_pools: ["dmz", "default"] };
    expect(bodyOf(g)).toEqual({ body: { max_concurrent_tasks: 20, max_retention_days: 365, max_artifact_bytes: 536870912000, max_run_duration: "24h", allowed_runner_pools: ["default", "dmz"] } });
  });

  it("refuse what the API would, before anything is sent", () => {
    expect(bodyOf({ ...formOf(undefined), max_concurrent_tasks: "0" })).toMatchObject({ field: "max_concurrent_tasks" });
    expect(bodyOf({ ...formOf(undefined), max_run_duration: "1 day" })).toMatchObject({ field: "max_run_duration" });
    expect(bodyOf({ ...formOf(undefined), max_runs_per_hour: "2.5" })).toMatchObject({ field: "max_runs_per_hour" });
    expect(bodyOf({ ...formOf(undefined), max_retention_days: "0" })).toMatchObject({ field: "max_retention_days" });
  });

  it("read on one line, what bounds nothing left out", () => {
    expect(summary({ max_concurrent_tasks: 20, max_retention_days: 180, allowed_runner_pools: ["default", "dmz"] })).toBe("20 tasks at once · kept 180 days · pools default, dmz");
    expect(summary({ max_concurrent_tasks: 40, max_retention_days: 90, max_artifact_bytes: 536870912000, max_run_duration: "24h" })).toBe("40 tasks at once · 500 GiB of artifacts · kept 90 days · runs 24h at most · every pool accepting it");
    expect(summary({ max_concurrent_tasks: 20 })).toBe("20 tasks at once · every pool accepting it");
  });
});

describe("the users, for an administrator", () => {
  it("adds a user, its enrolment link shown once, and reads the users again", async () => {
    const s = scenario("dana");
    s["POST /api/v1/users"] = {
      status: 201,
      body: { user: { kind: "user", login: "erin", display_name: "Erin Lowe", admin: false, suspended: false, created_at: "2026-10-01T06:00:00Z" }, enrolment: { link: "https://agentiik.example.com/auth/enrol#code=agkenrol_x", expires_at: "2026-10-02T06:00:00Z" } },
    };
    const { asked } = open("/users", s);
    await fireEvent.click(await screen.findByRole("button", { name: "Add a user" }));
    const form = screen.getByRole("form", { name: "Add a user" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Login" }), { target: { value: "erin" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Given name" }), { target: { value: "Erin " } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Family name" }), { target: { value: "Lowe" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Email" }), { target: { value: "erin.lowe@example.com" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("https://agentiik.example.com/auth/enrol#code=agkenrol_x")).toBeTruthy();
    expect(sent(asked, "POST /api/v1/users")).toEqual([{ login: "erin", given_name: "Erin", family_name: "Lowe", email: "erin.lowe@example.com" }]);
    expect(asked.filter((a) => a.key === "GET /api/v1/users")).toHaveLength(2);
  });

  it("gives a user an email address, and removes it with an empty one", async () => {
    const s = scenario("dana");
    s["PATCH /api/v1/users/carol"] = { status: 200, body: { kind: "user", login: "carol", display_name: "Carol Diaz", email: "carol@example.com", admin: false, suspended: false } };
    const { asked } = open("/users", s);
    const row = (await screen.findByText("carol", { selector: "td .login" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Email" }));
    const form = screen.getByRole("form", { name: "Email address" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Email" }), { target: { value: " carol@example.com" } });
    await fireEvent.submit(form);
    await waitFor(() => expect(sent(asked, "PATCH /api/v1/users/carol")).toEqual([{ email: "carol@example.com" }]));
    await waitFor(() => expect(screen.queryByRole("form", { name: "Email address" })).toBeNull());
    const button = within(row).getByRole("button", { name: "Email" }) as HTMLButtonElement;
    await waitFor(() => expect(button.disabled).toBe(false));
    await fireEvent.click(button);
    const again = screen.getByRole("form", { name: "Email address" });
    await fireEvent.input(within(again).getByRole("textbox", { name: "Email" }), { target: { value: "" } });
    await fireEvent.submit(again);
    await waitFor(() => expect(sent(asked, "PATCH /api/v1/users/carol")).toEqual([{ email: "carol@example.com" }, { email: "" }]));
  });

  it("removes a user on a second click, and says what the API refused as it said it", async () => {
    const s = scenario("dana");
    s["DELETE /api/v1/users/carol"] = { status: 409, body: { error: "carol's personal namespace holds 3 workflows" } };
    const { asked } = open("/users", s);
    const row = (await screen.findByText("carol", { selector: "td .login" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Carol's personal namespace holds 3 workflows.")).toBeTruthy();
    const own = (await screen.findByText("dana", { selector: "td .login" })).closest("tr")!;
    expect(within(own).queryByRole("button", { name: "Remove" })).toBeNull();
  });
});

describe("the groups, for an administrator", () => {
  it("lists each group with its members, and creates one with its first members", async () => {
    const s = scenario("dana");
    s["POST /api/v1/groups"] = { status: 201, body: { kind: "group", name: "platform", members: ["alice", "dana"] } };
    const { asked } = open("/groups", s);
    const row = (await screen.findByText("group:research", { selector: "td" })).closest("tr")!;
    expect(within(row).getAllByRole("button", { name: /^Take .* out of group:research$/ }).map((b) => b.getAttribute("aria-label"))).toEqual(["Take carol out of group:research", "Take dana out of group:research"]);
    await fireEvent.click(screen.getByRole("button", { name: "New group" }));
    const form = screen.getByRole("form", { name: "Create a group" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "platform" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "First members, by login" }), { target: { value: "alice, dana" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("group:platform created.")).toBeTruthy();
    expect(sent(asked, "POST /api/v1/groups")).toEqual([{ name: "platform", members: ["alice", "dana"] }]);
  });

  it("puts a user in a group and takes one out, each touching no grant", async () => {
    const s = scenario("dana");
    s["PUT /api/v1/groups/team-ops/members/bob-martin"] = { status: 200, body: { kind: "group", name: "team-ops", members: ["bob-martin"] } };
    s["DELETE /api/v1/groups/research/members/carol"] = { status: 200, body: { kind: "group", name: "research", members: ["dana"] } };
    const { asked } = open("/groups", s);
    const form = await screen.findByRole("form", { name: "Add a member to group:team-ops" });
    await fireEvent.input(within(form).getByRole("combobox", { name: "Login to add to group:team-ops" }), { target: { value: "bob-martin" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("bob-martin added.")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Take carol out of group:research" }));
    expect(await screen.findByText("carol removed.")).toBeTruthy();
    expect(asked.filter((a) => a.key.startsWith("PUT") || a.key.startsWith("DELETE")).map((a) => a.key)).toEqual(["PUT /api/v1/groups/team-ops/members/bob-martin", "DELETE /api/v1/groups/research/members/carol"]);
  });

  it("removes a group on a second click, refused while it owns a namespace", async () => {
    const s = scenario("dana");
    s["DELETE /api/v1/groups/finance-leads"] = { status: 409, body: { error: "group:finance-leads owns finance: name another owner first" } };
    open("/groups", s);
    const row = (await screen.findByText("group:finance-leads", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Group:finance-leads owns finance: name another owner first.")).toBeTruthy();
  });
});

describe("the namespaces, for an administrator", () => {
  it("lists every namespace with its owner and quotas, and writes a namespace's quotas against the pools", async () => {
    const s = scenario("dana");
    s["PUT /api/v1/namespaces/finance/quotas"] = { status: 200, body: { max_concurrent_tasks: 30, max_retention_days: 180, max_run_duration: "24h", allowed_runner_pools: ["default", "dmz"] } };
    const { asked, place } = open("/namespaces", s);
    const row = (await screen.findByRole("button", { name: "finance" })).closest("tr")!;
    expect(row.textContent).toContain("group:finance-leads");
    expect(row.textContent).toContain("20 tasks at once · kept 180 days · pools default, dmz");
    await fireEvent.click(within(row).getByRole("button", { name: "finance" }));
    expect(place.query.get("namespace")).toBe("finance");
    const form = await screen.findByRole("form", { name: "Quotas of finance" });
    expect((within(form).getByRole("checkbox", { name: /^dmz/ }) as HTMLInputElement).checked).toBe(true);
    expect(within(form).getByRole("checkbox", { name: /^gpu/ }).closest("label")!.textContent).toContain("does not accept finance");
    await fireEvent.input(within(form).getByRole("textbox", { name: /Tasks at once/ }), { target: { value: "30" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: /Longest run/ }), { target: { value: "24h" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("Quotas saved.")).toBeTruthy();
    expect(sent(asked, "PUT /api/v1/namespaces/finance/quotas")).toEqual([{ max_concurrent_tasks: 30, max_retention_days: 180, max_run_duration: "24h", allowed_runner_pools: ["default", "dmz"] }]);
  });

  it("refuses a quota the API would before anything is sent", async () => {
    const { asked } = open("/namespaces?namespace=team-ops");
    const form = await screen.findByRole("form", { name: "Quotas of team-ops" });
    await fireEvent.input(within(form).getByRole("textbox", { name: /Longest run/ }), { target: { value: "a day" } });
    await fireEvent.submit(form);
    expect(await screen.findByText(/Longest run: a length as a step's timeout writes it/)).toBeTruthy();
    expect(asked.some((a) => a.key.startsWith("PUT"))).toBe(false);
  });

  it("creates a shared namespace with its owner, and removes one on a second click, never a personal one", async () => {
    const s = scenario("dana");
    s["POST /api/v1/namespaces"] = { status: 201, body: { name: "platform", kind: "shared", owner: "group:platform", quotas: { max_concurrent_tasks: 20 } } };
    s["DELETE /api/v1/namespaces/team-ops"] = { status: 204 };
    const { asked } = open("/namespaces", s);
    await fireEvent.click(await screen.findByRole("button", { name: "New namespace" }));
    const form = screen.getByRole("form", { name: "Create a namespace" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "platform" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Owner, a login or group:NAME" }), { target: { value: "group:platform" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("platform created.")).toBeTruthy();
    expect(sent(asked, "POST /api/v1/namespaces")).toEqual([{ name: "platform", kind: "shared", owner: "group:platform" }]);

    const personal = screen.getByRole("button", { name: "dana" }).closest("tr")!;
    expect(within(personal).queryByRole("button", { name: "Remove" })).toBeNull();
    const shared = screen.getByRole("button", { name: "team-ops" }).closest("tr")!;
    await fireEvent.click(within(shared).getByRole("button", { name: "Remove" }));
    await fireEvent.click(within(shared).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("team-ops removed.")).toBeTruthy();
    expect(asked.filter((a) => a.key.startsWith("DELETE")).map((a) => a.key)).toEqual(["DELETE /api/v1/namespaces/team-ops"]);
  });

  it("are reached from the sidebar, and offered to nobody else", async () => {
    const { place } = open("/dana/runs");
    const installation = await screen.findByRole("list", { name: "Administration" });
    await fireEvent.click(within(installation).getByRole("link", { name: "Users" }));
    expect(place.route).toEqual({ kind: "users" });
    await fireEvent.click(within(installation).getByRole("link", { name: "Namespaces" }));
    expect(place.route).toEqual({ kind: "namespaces" });
    cleanup();

    open("/groups", scenario("alice"));
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });
});

describe("the service accounts of the namespaces the caller owns", () => {
  it("lists them, the built-in one kept, and creates one in a namespace the caller owns", async () => {
    const s = scenario("alice");
    s["POST /api/v1/service-accounts"] = { status: 201, body: { kind: "service_account", namespace: "alice", name: "nightly", created_by: "alice", created_at: "2026-10-01T06:00:00Z" } };
    const { asked } = open("/me/service-accounts", s);
    const pane = await screen.findByRole("region", { name: "Service accounts" });
    const builtIn = (await within(pane).findByText("alice/agentiik", { selector: "td" })).closest("tr")!;
    expect(builtIn.textContent).toContain("built in");
    expect(within(builtIn).queryByRole("button")).toBeNull();
    const form = screen.getByRole("form", { name: "Create a service account" });
    expect([...within(form).getByRole("combobox", { name: "In" }).querySelectorAll("option")].map((o) => o.value)).toEqual(["alice"]);
    await fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), { target: { value: "nightly" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("alice/nightly created.")).toBeTruthy();
    expect(sent(asked, "POST /api/v1/service-accounts")).toEqual([{ namespace: "alice", name: "nightly" }]);
  });

  it("removes one on a second click, with its tokens and grants", async () => {
    const s = scenario("alice");
    s["DELETE /api/v1/service-accounts/alice/deploy-bot"] = { status: 204 };
    const { asked } = open("/me/service-accounts", s);
    const row = (await screen.findByText("alice/deploy-bot", { selector: "td" })).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("alice/deploy-bot removed.")).toBeTruthy();
  });
});
