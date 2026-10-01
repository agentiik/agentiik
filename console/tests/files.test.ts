import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

function open(path: string, search = "", s: Scenario = scenario("alice"), sent: { path: string; body: unknown }[] = [], asked: string[] = []) {
  const answer = answering(s, asked);
  const api = connect("http://stand-in/", async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === "POST") sent.push({ path: new URL(request.url).pathname, body: await request.clone().json().catch(() => null) });
    return answer(request);
  });
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

const head = "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f";
const older = "9b1f3d5e7a0c2e4f6b8d0a1c3e5f7b9d2a4c6e8f";

describe("a workflow's files", () => {
  it("list the tree of the default branch as folders, the entry point open in JetBrains Mono and numbered", async () => {
    open("/finance/workflows/monthly-invoicing/files");
    const list = await screen.findByRole("region", { name: "Files at main" });
    expect(within(list).getByText("6 files at")).toBeTruthy();
    const rows = within(list).getAllByRole("button").map((b) => (b.classList.contains("folder") ? b.textContent!.replace(/^[▸▾]/, "") : [...b.querySelectorAll("span")].map((x) => x.textContent).join(" ")));
    expect(rows).toEqual(["assets", "schemas", "scripts", expect.stringMatching(/^agentiik\.yaml \d\.\d KiB$/), "common-bricks.yaml 99 B"]);
    const file = await screen.findByRole("list", { name: "agentiik.yaml at main" });
    expect(file.classList.contains("mono")).toBe(true);
    expect(within(file).getAllByRole("listitem")[0]!.textContent).toBe("1# finance/monthly-invoicing");
    expect(screen.getByText(head.slice(0, 7))).toBeTruthy();
  });

  it("open a folder, show a file chosen, and say a binary file is not drawn", async () => {
    const place = open("/finance/workflows/monthly-invoicing/files");
    await fireEvent.click(await screen.findByRole("button", { name: "scripts" }));
    await fireEvent.click(await screen.findByRole("button", { name: /normalize\.py/ }));
    expect(place.query.get("path")).toBe("scripts/normalize.py");
    const file = await screen.findByRole("list", { name: "scripts/normalize.py at main" });
    expect(within(file).getAllByRole("listitem")[0]!.textContent).toBe("1import json");
    expect(screen.getByText(/mode 0755/)).toBeTruthy();
    const download = screen.getByRole("link", { name: "Download" });
    expect(download.getAttribute("href")).toBe(`api/v1/finance/workflows/monthly-invoicing/tree/${head}?path=scripts%2Fnormalize.py`);

    place.narrow(new URLSearchParams({ path: "assets/logo.png" }));
    expect(await screen.findByText(/Not drawn: binary, 17 B/)).toBeTruthy();
  });

  it("switch to another ref, a branch holding a slash, and say a ref that names nothing", async () => {
    const asked: string[] = [];
    const place = open("/finance/workflows/monthly-invoicing/files", "", scenario("alice"), [], asked);
    const input = await screen.findByRole("combobox", { name: "Branch, tag or commit" });
    await fireEvent.input(input, { target: { value: "feature/vat-rounding" } });
    await fireEvent.submit(input.closest("form")!);
    expect(place.query.get("ref")).toBe("feature/vat-rounding");
    await screen.findByRole("region", { name: "Files at feature/vat-rounding" });
    expect(asked).toContain("GET /api/v1/finance/workflows/monthly-invoicing/tree/feature%2Fvat-rounding");
    const file = await screen.findByRole("list", { name: "agentiik.yaml at feature/vat-rounding" });
    await waitFor(() => expect(file.textContent).toContain("vat_scale: 4"));

    place.narrow(new URLSearchParams({ ref: "nowhere" }));
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "No branch, tag or version of monthly-invoicing is named nowhere.");
  });

  it("compare two refs over the whole tree, and the file chosen line by line", async () => {
    open("/finance/workflows/monthly-invoicing/files", `?against=${older}`);
    const differ = await screen.findByRole("region", { name: "What differs" });
    expect(within(differ).getByText(`4 files differ from ${older.slice(0, 7)} to main`)).toBeTruthy();
    const rows = within(differ).getAllByRole("button").map((b) => [...b.querySelectorAll("span")].map((x) => x.textContent).join(" "));
    expect(rows).toEqual(["agentiik.yaml modified", "schemas/customer.json added", "schemas/customers.json modified", "scripts/normalize.py modified"]);
    const diff = await screen.findByRole("region", { name: "agentiik.yaml" });
    await waitFor(() => expect(diff.querySelectorAll("li.removed").length).toBe(1));
    expect(diff.querySelector("li.removed")!.textContent).toContain("rounding: per_invoice");
    expect([...diff.querySelectorAll("li.added")].map((l) => l.textContent?.trim())).toEqual(["57+      rounding: per_line", "66+    merge: wait_all"]);
    expect(within(diff).getByText("+2")).toBeTruthy();
  });

  it("say a file whose bytes are the same and whose mode changed", async () => {
    const place = open("/finance/workflows/monthly-invoicing/files", `?against=${older}&path=scripts%2Fnormalize.py`);
    expect(place.query.get("path")).toBe("scripts/normalize.py");
    expect(await screen.findByText("mode 0644 to 0755")).toBeTruthy();
    expect(await screen.findByText("The bytes are the same; only the mode changed.")).toBeTruthy();
  });

  it("run the ref shown, its name in the run form, under workflow:run", async () => {
    const sent: { path: string; body: unknown }[] = [];
    open("/finance/workflows/monthly-invoicing/files", "?ref=feature%2Fvat-rounding", scenario("alice"), sent);
    await fireEvent.click(await screen.findByRole("button", { name: "Run feature/vat-rounding" }));
    await waitFor(() => expect((document.getElementById("run-ref") as HTMLInputElement).value).toBe("feature/vat-rounding"));
  });

  it("tell how to fill a repository nothing was pushed to", async () => {
    open("/alice/workflows/vat-reconciliation/files");
    expect(await screen.findByText("git clone https://agentiik.example.com/alice/vat-reconciliation.git")).toBeTruthy();
  });
});

describe("a namespace's workflows", () => {
  it("are those its runs name, said as such, each with its latest run", async () => {
    open("/finance/workflows");
    expect(await screen.findByText(/The API lists no namespace's workflows/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "monthly-invoicing" }).getAttribute("href")).toBe("/finance/workflows/monthly-invoicing");
  });

  it("gain a new one, empty, which opens on how to fill it", async () => {
    const sent: { path: string; body: unknown }[] = [];
    const place = open("/alice/workflows", "", scenario("alice"), sent);
    await fireEvent.input(await screen.findByLabelText(/^Name/), { target: { value: "vat-reconciliation" } });
    await fireEvent.click(screen.getByRole("checkbox"));
    await fireEvent.click(screen.getByRole("button", { name: "Create vat-reconciliation" }));
    await waitFor(() => expect(sent).toEqual([{ path: "/api/v1/alice/workflows", body: { name: "vat-reconciliation", protected: true } }]));
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "alice", view: "workflows", workflow: "vat-reconciliation", tab: "files" }));
  });

  it("refuse a name already taken as the API says it, and offer nothing to create without workflow:write", async () => {
    const s = scenario("alice");
    s["POST /api/v1/alice/workflows"] = { status: 409, body: { error: "a workflow of that name is in the namespace" } };
    open("/alice/workflows", "", s);
    await fireEvent.input(await screen.findByLabelText(/^Name/), { target: { value: "report" } });
    await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
    expect((await screen.findByRole("alert")).textContent).toBe("A workflow named report is already in alice, or one deleted under that name is still being purged.");
  });
});
