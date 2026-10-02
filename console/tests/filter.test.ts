import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { filtered } from "../src/lib/palette";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

function open(path: string, search = "", who = "alice") {
  const api = connect("http://stand-in/", answering(scenario(who)));
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

const names = (region: HTMLElement) => within(region).queryAllByRole("link").map((a) => a.textContent?.trim());

describe("filtered", () => {
  it("keeps the rows every word matches, the closest first and in their order among equals", () => {
    const rows = ["monthly-invoicing", "dunning-reminders", "ledger-export", "vat-reconcile"];
    expect(filtered(rows, "", (r) => r)).toEqual(rows);
    expect(filtered(rows, "re", (r) => r)).toEqual(["dunning-reminders", "vat-reconcile", "ledger-export"]);
    expect(filtered(rows, "led exp", (r) => r)).toEqual(["ledger-export"]);
    expect(filtered(rows, "zz", (r) => r)).toEqual([]);
  });
});

describe("the Filter field", () => {
  it("narrows a namespace's workflows as letters are typed, and keeps them in the address", async () => {
    const place = open("/finance/workflows");
    const pane = await screen.findByRole("region", { name: "Workflows of finance" });
    await within(pane).findByRole("link", { name: "monthly-invoicing" });
    const field = screen.getByRole("searchbox", { name: "Filter the workflows" });
    await fireEvent.input(field, { target: { value: "dun" } });
    await waitFor(() => expect(names(pane).filter((n) => !n?.startsWith("01"))).toEqual(["dunning-reminders"]));
    expect(place.query.get("q")).toBe("dun");
    await fireEvent.input(field, { target: { value: "qqq" } });
    expect(await within(pane).findByText("Nothing matches.")).toBeTruthy();
    await fireEvent.input(field, { target: { value: "" } });
    await waitFor(() => expect(place.query.has("q")).toBe(false));
    expect(within(pane).queryByText("Nothing matches.")).toBeNull();
  });

  it("opens narrowed where the address says so", async () => {
    open("/finance/workflows", "?q=vat");
    const pane = await screen.findByRole("region", { name: "Workflows of finance" });
    await within(pane).findByRole("link", { name: "vat-reconcile" });
    expect(names(pane).filter((n) => !n?.startsWith("01"))).toEqual(["vat-reconcile"]);
    expect((screen.getByRole("searchbox", { name: "Filter the workflows" }) as HTMLInputElement).value).toBe("vat");
  });

  it("takes the cursor on / and gives it back on esc", async () => {
    open("/finance/workflows");
    await screen.findByRole("link", { name: "monthly-invoicing" });
    const field = screen.getByRole("searchbox", { name: "Filter the workflows" });
    await fireEvent.keyDown(window, { key: "/" });
    expect(document.activeElement).toBe(field);
    await fireEvent.keyDown(field, { key: "Escape" });
    expect(document.activeElement).not.toBe(field);
  });

  it("finds a user by their email as well as their login", async () => {
    open("/users", "", "dana");
    const pane = await screen.findByRole("region", { name: "Users" });
    await within(pane).findByText("carol");
    const field = screen.getByRole("searchbox", { name: "Filter the users" });
    const logins = () => [...pane.querySelectorAll("tbody .login")].map((e) => e.textContent);
    await fireEvent.input(field, { target: { value: "example.com" } });
    expect(logins()).toEqual(["alice", "dana"]);
    await fireEvent.input(field, { target: { value: "martin" } });
    expect(logins()).toEqual(["alice", "bob-martin"]);
  });
});
