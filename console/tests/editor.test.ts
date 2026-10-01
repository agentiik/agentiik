import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

function open(search = "", s = scenario("alice")) {
  const api = connect("http://stand-in/", answering(s));
  const place = new Place({ pathname: "/finance/workflows/monthly-invoicing", search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

const text = async () => ((await screen.findByLabelText("agentiik.yaml, as it is edited")) as HTMLTextAreaElement).value;

describe("the visual editor", () => {
  it("opens from the workflow's page under workflow:write, and the address says so", async () => {
    const place = open();
    await fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    expect(place.query.get("edit")).toBe("1");
    expect(await screen.findByText("No change")).toBeTruthy();
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
    await fireEvent.click(await screen.findByRole("button", { name: "Remove normalize" }));
    expect(await screen.findByText("invoice, archive take input from normalize. Remove those edges first.")).toBeTruthy();
    expect(screen.getByText("No change")).toBeTruthy();
  });

  it("follows the text typed, and keeps the last graph that read while the text does not", async () => {
    open("?edit=1");
    const area = await screen.findByLabelText("agentiik.yaml, as it is edited");
    const before = (area as HTMLTextAreaElement).value;
    await fireEvent.input(area, { target: { value: before.replace("  archive:\n", "  archive:\n    retries: many\n") } });
    await waitFor(() => expect(screen.getByText(/^\d+ problems?$/)).toBeTruthy());
    expect(screen.getByRole("button", { name: /^Step archive/ })).toBeTruthy();
    await fireEvent.input(area, { target: { value: before.replace("  archive:\n", "  archived:\n").replace("step: archive,", "step: archived,") } });
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
