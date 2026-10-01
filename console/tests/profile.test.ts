import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { changed, localTime, photoOf, profileOf, type User } from "../src/lib/profile";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario } from "./scenario";

// The installation alice's profile is written to: each request kept with its type and its body as
// sent, JSON or the bytes of a photo, and GET /api/v1/me answered as the last write left it.
type Answer = { status: number; body?: unknown };

function installation(extra: Record<string, Answer> = {}) {
  const s = scenario("alice") as Record<string, Answer>;
  let me = structuredClone(s["GET /api/v1/me"]!.body) as { user: User };
  const asked: { key: string; type: string | null; body: unknown; bytes: number }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const type = request.headers.get("Content-Type");
    const raw = request.method === "GET" ? new ArrayBuffer(0) : await request.arrayBuffer();
    const body = type?.startsWith("application/json") && raw.byteLength ? JSON.parse(new TextDecoder().decode(raw)) : undefined;
    asked.push({ key, type, body, bytes: raw.byteLength });
    const json = (status: number, b?: unknown) => new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { "Content-Type": "application/json" } });
    if (extra[key]) return json(extra[key]!.status, extra[key]!.body);
    if (key === "GET /api/v1/me") return json(200, me);
    if (key === "PATCH /api/v1/me") {
      me = { ...me, user: { ...me.user, ...(body as object) } };
      return json(200, me);
    }
    if (key === "PUT /api/v1/me/avatar") {
      me = { ...me, user: { ...me.user, avatar_updated_at: "2026-10-01T06:05:00Z" } };
      return json(204);
    }
    if (key === "DELETE /api/v1/me/avatar") {
      me = { ...me, user: { ...me.user, avatar_updated_at: null } };
      return json(204);
    }
    const recorded = s[`${key}${url.search}`] ?? s[key];
    return recorded ? json(recorded.status, recorded.body) : json(404, { error: "no such thing, or not yours" });
  };
  return { asked, fetcher };
}

function open(path: string, extra: Record<string, Answer> = {}) {
  const { asked, fetcher } = installation(extra);
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

describe("a profile", () => {
  const alice = { kind: "user", login: "alice", display_name: "Alice Martin", admin: false, suspended: false, given_name: "Alice", family_name: "", title: "", location: "", timezone: "", bio: "", avatar_updated_at: null } as User;

  it("sends only what changed, each field trimmed", () => {
    const before = profileOf(alice);
    expect(changed(before, { ...before, title: "  Technical lead ", family_name: "Martin" })).toEqual({ title: "Technical lead", family_name: "Martin" });
    expect(changed(before, { ...before, given_name: "Alice " })).toEqual({});
  });

  it("reads the photo at an address that changes each time it is set, and none without one", () => {
    expect(photoOf({ user: alice } as never)).toBeUndefined();
    expect(photoOf({ user: { ...alice, avatar_updated_at: "2026-09-28T10:15:00Z" } } as never)).toBe("api/v1/me/avatar?v=2026-09-28T10%3A15%3A00Z");
  });

  it("says the time where somebody is, and nothing for a zone the browser does not know", () => {
    expect(localTime("Europe/Paris", Date.parse("2026-10-01T06:02:30Z"))).toBe("08:02");
    expect(localTime("Nowhere/At_all", 0)).toBeUndefined();
    expect(localTime("", 0)).toBeUndefined();
  });
});

describe("the caller's profile in the console", () => {
  it("opens from the account, and writes what was changed alone", async () => {
    const { asked, place } = open("/");
    await fireEvent.click(await screen.findByRole("link", { name: "Your account" }));
    expect(place.route).toEqual({ kind: "account", tab: "profile" });
    const form = await screen.findByRole("form", { name: "Your profile" });
    expect((within(form).getByRole("textbox", { name: /^Display name/ }) as HTMLInputElement).value).toBe("Alice Martin");
    await fireEvent.input(within(form).getByRole("textbox", { name: "Family name" }), { target: { value: "Martin" } });
    await fireEvent.input(within(form).getByRole("textbox", { name: /^Title/ }), { target: { value: "Technical lead " } });
    await fireEvent.submit(form);
    expect(await screen.findByText("Saved.")).toBeTruthy();
    expect(asked.filter((a) => a.key === "PATCH /api/v1/me").map((a) => a.body)).toEqual([{ family_name: "Martin", title: "Technical lead" }]);
    expect(asked.filter((a) => a.key === "GET /api/v1/me").length).toBeGreaterThan(1);
  });

  it("sends a photo as its own bytes, refuses one the API would, and removes it", async () => {
    const { asked } = open("/me/profile");
    const picker = (await screen.findByLabelText("A photo, a PNG or a JPEG")) as HTMLInputElement;

    const gif = new File([new Uint8Array([71, 73, 70])], "me.gif", { type: "image/gif" });
    await fireEvent.change(picker, { target: { files: [gif] } });
    expect(await screen.findByText("A photo must be a PNG or a JPEG file.")).toBeTruthy();
    expect(asked.some((a) => a.key === "PUT /api/v1/me/avatar")).toBe(false);

    const png = new File([new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10])], "me.png", { type: "image/png" });
    await fireEvent.change(picker, { target: { files: [png] } });
    expect(await screen.findByText("Photo updated.")).toBeTruthy();
    const put = asked.find((a) => a.key === "PUT /api/v1/me/avatar")!;
    expect(put.type).toBe("image/png");
    expect(put.bytes).toBe(8);

    await fireEvent.click(await screen.findByRole("button", { name: "Remove it" }));
    expect(await screen.findByText("Photo removed.")).toBeTruthy();
    await waitFor(() => expect(screen.getByRole("button", { name: "Choose a photo" })).toBeTruthy());
  });

  it("says why a service account has none", async () => {
    const s = scenario("alice") as Record<string, Answer>;
    const me = structuredClone(s["GET /api/v1/me"]!.body) as Record<string, unknown>;
    delete me.user;
    me.principal = "finance/deployer";
    me.service_account = { kind: "service_account", namespace: "finance", name: "deployer", created_at: "2026-09-28T08:00:00Z" };
    open("/me/profile", { "GET /api/v1/me": { status: 200, body: me } });
    expect(await screen.findByText("Service accounts have no profile.")).toBeTruthy();
    expect(screen.queryByRole("form", { name: "Your profile" })).toBeNull();
  });
});
