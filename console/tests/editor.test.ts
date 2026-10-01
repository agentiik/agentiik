import { cleanup as cleanupAll, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { EditorView } from "@codemirror/view";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { commitFile } from "../src/lib/git/commit";
import { Told } from "../src/lib/problem";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

// The commit goes over git, which tests/git.test.ts holds to git's own; here it is a stand-in, so
// that what the editor sends it and what it does with the answer are what is tested.
vi.mock("../src/lib/git/commit", () => ({ commitFile: vi.fn() }));
const committed = vi.mocked(commitFile);
// A function a beforeEach returns is called when the test ends, and mockReset returns the mock: the
// braces keep the stand-in from being called once more, after the test, with what it was last given.
beforeEach(() => {
  committed.mockReset();
});

function open(search = "", s = scenario("alice")) {
  const api = connect("http://stand-in/", answering(s));
  const place = new Place({ pathname: "/finance/workflows/monthly-invoicing", search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

// The YAML editor is CodeMirror, read and typed into through its view, as a person's keys reach it.
const editorView = async () => EditorView.findFromDOM((await screen.findByLabelText("agentiik.yaml, as it is edited")) as HTMLElement)!;
const text = async () => (await editorView()).state.doc.toString();
const type = async (next: string) => {
  const view = await editorView();
  view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: next }, userEvent: "input.type" });
};

describe("the visual editor", () => {
  it("opens from the workflow's page under workflow:write, and the address says so", async () => {
    const place = open();
    await fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    expect(place.query.get("edit")).toBe("1");
    // The editor is a chunk of its own, imported when it is opened, which a machine running every
    // file at once may take longer than a second to load.
    expect(await screen.findByText("No change", {}, { timeout: 5000 })).toBeTruthy();
    expect(await text()).toContain("  invoice:\n    extends: .api-brick");
    await fireEvent.click(screen.getByRole("button", { name: "Stop editing" }));
    expect(place.query.get("edit")).toBeNull();
  });

  it("adds a step from a dialog, writing it at the end of steps, and draws it", async () => {
    open("?edit=1");
    await fireEvent.click(await screen.findByRole("button", { name: "Add a step" }));
    const dialog = within(await screen.findByRole("form", { name: "Add a step" }));
    await fireEvent.input(dialog.getByLabelText("Name"), { target: { value: "notify" } });
    await fireEvent.input(dialog.getByLabelText("Image"), { target: { value: "ghcr.io/acme/agk-notify:1.0.0" } });
    await fireEvent.input(dialog.getByLabelText(/Output ports/), { target: { value: "ok" } });
    await fireEvent.submit(screen.getByRole("form", { name: "Add a step" }));
    expect(await text()).toMatch(/outputs: \[ok\]\n\n {2}notify:\n {4}image: ghcr.io\/acme\/agk-notify:1.0.0\n {4}outputs: \[ok\]\n$/);
    expect(screen.getByText("4 lines added")).toBeTruthy();
    expect(await screen.findByRole("button", { name: /^Step notify/ })).toBeTruthy();
    expect(screen.getByText("notify is added.")).toBeTruthy();
  });

  it("connects a port to the step chosen, takes the edge away, and undoes each edit in turn", async () => {
    open("?edit=1&step=archive");
    const form = within(await screen.findByRole("form", { name: "Connect a port to archive" }));
    await fireEvent.change(form.getByLabelText("The port it takes"), { target: { value: "normalize.ok" } });
    await fireEvent.input(form.getByLabelText("The input port it arrives on"), { target: { value: "normalized" } });
    await fireEvent.click(form.getByRole("button", { name: "Connect" }));
    expect(await text()).toContain("      - { step: normalize, port: rejected, as: rejected }\n      - { step: normalize, port: ok, as: normalized }\n");
    expect(screen.getByText("1 line added")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Take the edge from invoice.out away" }));
    expect(await text()).not.toContain("{ step: invoice, port: out, as: invoices }");
    expect(screen.getByText("1 line added, 1 removed")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    expect(await text()).toContain("{ step: invoice, port: out, as: invoices }");
    await fireEvent.click(screen.getByRole("button", { name: "Undo every change" }));
    expect(await screen.findByText("No change")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Undo" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("changes how a step runs, writing the one line it changes", async () => {
    open("?edit=1&step=invoice");
    await fireEvent.change(await screen.findByLabelText("fan_out"), { target: { value: "batch" } });
    expect(await text()).toContain("      fan_out: batch(10)\n      max_parallel: 8\n");
    await fireEvent.change(screen.getByLabelText("max_parallel"), { target: { value: "" } });
    expect(await text()).not.toContain("max_parallel");
    expect(screen.getByText("1 line added, 2 removed")).toBeTruthy();
  });

  it("refuses an edit the language would refuse, saying why, and leaves the file as it was", async () => {
    open("?edit=1&step=normalize");
    await fireEvent.click(await screen.findByRole("button", { name: "Remove the step" }));
    expect(await screen.findByText("invoice, archive take input from normalize. Remove those edges first.")).toBeTruthy();
    expect(screen.getByText("No change")).toBeTruthy();
  });

  it("follows the text typed, and keeps the last graph that read while the text does not", async () => {
    open("?edit=1");
    const before = await text();
    await type(before.replace("  archive:\n", "  archive:\n    retries: many\n"));
    await waitFor(() => expect(screen.getByText(/^\d+ problems?$/)).toBeTruthy());
    expect(screen.getByRole("button", { name: /^Step archive/ })).toBeTruthy();
    await type(before.replace("  archive:\n", "  archived:\n").replace("step: archive,", "step: archived,"));
    expect(await screen.findByRole("button", { name: /^Step archived/ })).toBeTruthy();
    expect(screen.getByText("valid")).toBeTruthy();
  });

  it("is not offered to a reader who may not write the workflow, nor open from the address", async () => {
    const s = scenario("alice");
    const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
    me.permissions.finance = me.permissions.finance!.filter((p) => p !== "workflow:write");
    open("?edit=1", s);
    await screen.findByRole("button", { name: "History" });
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("toolbar", { name: "The editor" })).toBeNull();
  });
});

describe("the YAML editor", () => {
  it("shows the file alone across the page, the address saying so, and back beside the graph", async () => {
    const place = open("?edit=1");
    await fireEvent.click(await screen.findByRole("button", { name: "YAML" }));
    expect(place.query.get("view")).toBe("yaml");
    expect(screen.getByRole("button", { name: "YAML" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.queryByRole("region", { name: "Graph" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add a step" })).toBeNull();
    expect(await text()).toContain("  invoice:\n    extends: .api-brick");
    await fireEvent.click(screen.getByRole("button", { name: "Graph" }));
    expect(place.query.get("view")).toBeNull();
    expect(await screen.findByRole("region", { name: "Graph" })).toBeTruthy();
  });

  it("lists each problem by its line, which puts the cursor there", async () => {
    open("?edit=1&view=yaml");
    const before = await text();
    await type(before.replace("  archive:\n", "  archive:\n    retries: many\n"));
    const line = await screen.findByRole("button", { name: /^line \d+$/ });
    await fireEvent.click(line);
    const view = await editorView();
    const at = view.state.doc.lineAt(view.state.selection.main.head).number;
    expect(line.textContent).toBe(`line ${at}`);
  });
});

describe("committing from the editor", () => {
  // alice holds workflow:write on finance and not grant:manage, which its protected main takes: she
  // is given it where a test commits onto main.
  function managing() {
    const s = scenario("alice");
    (s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> }).permissions.finance!.push("grant:manage");
    return s;
  }

  async function edited(s = scenario("alice")) {
    const place = open("?edit=1&view=yaml", s);
    const before = await text();
    await type(before.replace("  archive:\n", "  archive:\n    # kept ten years\n"));
    return { place, before };
  }

  it("is offered once the file has changed, and commits it onto the default branch as the person signed in", async () => {
    open("?edit=1");
    expect(((await screen.findByRole("button", { name: "Commit" })) as HTMLButtonElement).disabled).toBe(true);
    cleanupAll();
    const { place } = await edited(managing());
    committed.mockResolvedValue("b".repeat(40));
    await fireEvent.click(screen.getByRole("button", { name: "Commit" }));
    const form = screen.getByRole("form", { name: "Commit" });
    await fireEvent.input(within(form).getByRole("textbox", { name: "Message" }), { target: { value: "Say how long the archive keeps" } });
    expect((within(form).getByRole("radio", { name: "main" }) as HTMLInputElement).checked).toBe(true);
    await fireEvent.submit(form);
    await waitFor(() => expect(committed).toHaveBeenCalledOnce());
    const sent = committed.mock.calls[0]![0];
    expect(sent).toMatchObject({ parent: "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f", branch: "main", create: false, path: "agentiik.yaml", message: "Say how long the archive keeps", author: { name: "Alice Martin", email: "alice.martin@example.com" } });
    expect(sent.remote.url).toBe("https://agentiik.example.com/finance/monthly-invoicing.git");
    expect(sent.text).toContain("    # kept ten years\n");
    expect(await screen.findByText("Committed to main.")).toBeTruthy();
    expect(place.query.get("edit")).toBeNull();
  });

  it("commits onto a new branch alone where the default branch is protected from the caller, and opens the files at it", async () => {
    const { place } = await edited();
    committed.mockResolvedValue("c".repeat(40));
    await fireEvent.click(screen.getByRole("button", { name: "Commit" }));
    const form = screen.getByRole("form", { name: "Commit" });
    expect(within(form).queryByRole("radio")).toBeNull();
    await fireEvent.input(within(form).getByRole("textbox", { name: "The new branch" }), { target: { value: "keep-longer" } });
    await fireEvent.submit(form);
    await waitFor(() => expect(place.route).toMatchObject({ workflow: "monthly-invoicing", tab: "files" }));
    expect(committed.mock.calls[0]![0]).toMatchObject({ branch: "keep-longer", create: true });
    expect(place.query.get("ref")).toBe("keep-longer");
  });

  it("says what the push was refused for, as the repository said it, and marks the line it names", async () => {
    await edited(managing());
    committed.mockImplementation(async () => {
      throw new Told("monthly-invoicing refused 2224: edge-port-not-declared at agentiik.yaml:20:38 the edge takes done from normalize, which publishes the output ports ok, rejected");
    });
    await fireEvent.click(screen.getByRole("button", { name: "Commit" }));
    await fireEvent.submit(screen.getByRole("form", { name: "Commit" }));
    expect(await screen.findByText(/Could not commit to main\./)).toBeTruthy();
    expect(screen.getAllByText(/edge-port-not-declared at agentiik\.yaml:20:38/).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "line 20" })).toBeTruthy();
    expect(screen.getByRole("form", { name: "Commit" })).toBeTruthy();
  });
});
