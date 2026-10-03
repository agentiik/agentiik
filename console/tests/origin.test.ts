import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { elsewhere } from "../src/lib/origin";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

describe("a console opened away from the public URL", () => {
  it("names the same page at the public URL's origin, and nothing where it was opened there or no origin is named", () => {
    const declared = "https://agk.example.com";
    expect(elsewhere({ href: "http://10.0.0.4:8080/finance/workflows?q=x#top", origin: "http://10.0.0.4:8080", declared })).toBe("https://agk.example.com/finance/workflows?q=x#top");
    expect(elsewhere({ href: "https://agk.example.com/finance/workflows", origin: "https://agk.example.com", declared })).toBeNull();
    // An origin written with a trailing slash or a default port is the same origin.
    expect(elsewhere({ href: "https://agk.example.com/", origin: "https://agk.example.com", declared: "https://agk.example.com:443/" })).toBeNull();
    // The development server's page names no origin, and nothing is then known to be wrong.
    expect(elsewhere({ href: "http://localhost:5173/", origin: "http://localhost:5173", declared: null })).toBeNull();
    expect(elsewhere({ href: "http://localhost:5173/", origin: "http://localhost:5173", declared: "not an address" })).toBeNull();
  });

  it("says so above the sign-in page, linking the address that works", async () => {
    const s = scenario("alice");
    s["GET /api/v1/me"] = { status: 401, body: { error: "sign in first" } };
    const api = connect("http://stand-in/", answering(s));
    const place = new Place({ pathname: "/", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
    render(App, { api, session: new Session(api), place, version: "v0.7.0", passkeys: { unavailable: "" }, elsewhere: "https://agk.example.com/" });
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/Sign-in and changes work at https:\/\/agk\.example\.com only\./);
    expect(screen.getByRole("link", { name: "https://agk.example.com" }).getAttribute("href")).toBe("https://agk.example.com/");
  });
});
