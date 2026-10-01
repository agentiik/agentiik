import { fireEvent, render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect, Refusal } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { explain, refused } from "../src/lib/problem";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

describe("a failure told for a person", () => {
  it("says what failed, why in everyday words, what to do, and keeps the server's answer as the detail", () => {
    const failed = explain("load your account", new Refusal(500, "what the caller holds could not be read"));
    expect(failed).toEqual({
      what: "Could not load your account.",
      why: "Server error.",
      next: "Try again. If it keeps failing, tell your administrator.",
      detail: "500 “what the caller holds could not be read”",
      transient: true,
    });
  });

  it("tells each kind of refusal by its status, and the server's own sentence where it is the reason", () => {
    expect(explain("load the runs", new TypeError("Failed to fetch"))).toMatchObject({ why: "Agentiik is not reachable.", detail: "Failed to fetch", transient: true });
    expect(explain("revoke the grant", new Refusal(403, "you do not hold what this needs"))).toMatchObject({ why: "You do not have permission.", next: "", transient: false });
    expect(explain("load the run", new Refusal(404, "no such thing, or not yours"))).toMatchObject({ why: "Not found, or not shared with you." });
    expect(explain("add the user", new Refusal(409, "a user of that login exists"))).toMatchObject({ why: "A user of that login exists.", next: "", detail: "409" });
    expect(explain("save the secret", new Refusal(400, "env is not a store this installation reads"))).toMatchObject({ why: "Env is not a store this installation reads.", next: "" });
    expect(explain("sign in", new Refusal(429, "too many attempts"))).toMatchObject({ why: "Too many requests.", transient: true });
    expect(refused("set your photo", "A photo must be 1 MiB or smaller.")).toMatchObject({ what: "Could not set your photo.", why: "A photo must be 1 MiB or smaller.", detail: "" });
  });
});

describe("the console when the installation does not answer", () => {
  it("says what could not be loaded, why and what to do, with the server's answer below, and tries again on asking", async () => {
    const s = scenario("alice");
    const me = s["GET /api/v1/me"]!;
    s["GET /api/v1/me"] = { status: 500, body: { error: "what the caller holds could not be read" } };
    const api = connect("http://stand-in/", answering(s));
    const place = new Place({ pathname: "/", search: "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
    const session = new Session(api);
    render(App, { api, session, place, version: "v0.6.0", passkeys: { unavailable: "" } });
    expect(await screen.findByText("Agentiik is not available")).toBeTruthy();
    expect(screen.getByText("Could not load your account.")).toBeTruthy();
    expect(screen.getByText(/^Server error\./)).toBeTruthy();
    expect(screen.getByText("500 “what the caller holds could not be read”")).toBeTruthy();
    s["GET /api/v1/me"] = me;
    await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("navigation", { name: "Where you are" })).toBeTruthy();
  });
});
