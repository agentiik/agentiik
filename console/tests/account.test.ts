import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { listOf, scopeOf, type Token } from "../src/lib/credentials";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import type { Passkeys } from "../src/views/SignIn.svelte";
import { scenario } from "./scenario";

// An answer of the installation, and a route answered several times in turn, the last answer
// standing for every request after it.
type Answer = { status: number; body?: unknown; headers?: Record<string, string> };
type Answers = Record<string, Answer | Answer[]>;

function installation(answers: Answers) {
  const asked: { key: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const text = request.method === "GET" ? "" : await request.text();
    asked.push({ key, body: text ? JSON.parse(text) : undefined });
    const held = answers[key];
    const answer: Answer = Array.isArray(held) ? (held.length > 1 ? held.shift()! : held[0]!) : (held ?? { status: 404, body: { error: "no such thing, or not yours" } });
    return new Response(answer.body === undefined ? null : JSON.stringify(answer.body), {
      status: answer.status,
      headers: { "Content-Type": "application/json", ...answer.headers },
    });
  };
  return { asked, fetcher };
}

// alice's installation, with what her account holds.
function alice(extra: Answers = {}, admin = false): Answers {
  const s: Answers = { ...scenario("alice") };
  if (admin) {
    const me = structuredClone((s["GET /api/v1/me"] as Answer).body) as { admin: boolean };
    me.admin = true;
    s["GET /api/v1/me"] = { status: 200, body: me };
  }
  return {
    ...s,
    "GET /api/v1/auth/policy": { status: 200, body: { password: "allowed", passkey: "optional", user_verification: "required", device_bound_only: false, min_passkeys: 2 } },
    "GET /api/v1/me/credentials": {
      status: 200,
      body: {
        credentials: [
          { type: "password", id: "01M2AAZ9G62NQXFAFCXKRPJEH5", created_at: "2026-09-27T08:55:00Z", last_used_at: "2026-09-27T12:30:00Z" },
          { type: "totp", id: "01M2AB3K5Q7R9T1V3X5Z7B9D1F", created_at: "2026-09-27T09:05:00Z" },
          { type: "passkey", id: "Q2hyb21lUGFzc2tleTAx", label: "work laptop", backup_eligible: false, backup_state: false, kind: "device-bound", created_at: "2026-09-27T09:00:00Z" },
          { type: "passkey", id: "aVBob25lUGFzc2tleQ", label: "phone", backup_eligible: true, backup_state: true, kind: "synced", created_at: "2026-09-27T09:02:00Z", last_used_at: "2026-09-27T12:30:00Z" },
        ],
      },
    },
    "GET /api/v1/auth/tokens": {
      status: 200,
      body: {
        tokens: [
          { id: "01M2AD1R3T5W7Y9A1C3E5G7J9M", principal: "alice", device_label: "agk on alice-laptop", created_at: "2026-09-27T09:00:00Z", expires_at: "2026-12-26T09:00:00Z", last_used_at: "2026-09-29T08:00:00Z" },
          { id: "01M2AD1R3T5W7Y9A1C3E5G7J9N", principal: "finance/deployer", scope: { permissions: ["workflow:run"], within: ["finance/monthly-invoicing"] }, created_at: "2026-09-28T09:00:00Z", expires_at: "2026-10-28T09:00:00Z" },
        ],
      },
    },
    "GET /api/v1/service-accounts": {
      status: 200,
      body: {
        service_accounts: [
          { kind: "service_account", namespace: "finance", name: "agentiik", created_at: "2026-09-27T08:00:00Z" },
          { kind: "service_account", namespace: "finance", name: "deployer", created_at: "2026-09-28T08:00:00Z" },
        ],
      },
    },
    ...extra,
  };
}

function open(path: string, answers: Answers, passkeys: Passkeys = { unavailable: "" }) {
  const { asked, fetcher } = installation(answers);
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys });
  return { asked, place };
}

// An authenticator that makes and offers passkeys at once, as a virtual one does, keeping what it
// was asked for.
function authenticator() {
  const asked: { create: CredentialCreationOptions[]; get: CredentialRequestOptions[] } = { create: [], get: [] };
  const made = (kind: string) => ({ type: "public-key", id: kind, toJSON: () => ({ id: kind, rawId: kind, type: "public-key", response: {} }) });
  const credentials = {
    create: async (o: CredentialCreationOptions) => {
      asked.create.push(o);
      return made("bmV3UGFzc2tleQ");
    },
    get: async (o: CredentialRequestOptions) => {
      asked.get.push(o);
      return made("aVBob25lUGFzc2tleQ");
    },
  } as unknown as CredentialsContainer;
  return { asked, credentials };
}

const creation = {
  ceremony: "registration",
  options: {
    rp: { id: "stand-in", name: "Agentiik" },
    user: { id: "YWxpY2U", name: "alice", displayName: "Alice Martin" },
    challenge: "azJNNGVUQTI5VGdScFJQSGE1bXNjdEh5TXFjUTRMNUdEY1NyZlBSTjd1WQ",
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
    excludeCredentials: [{ type: "public-key", id: "Q2hyb21lUGFzc2tleTAx" }],
  },
};
const recorded = { type: "passkey", id: "bmV3UGFzc2tleQ", label: "desk", backup_eligible: false, backup_state: false, kind: "device-bound", created_at: "2026-10-01T09:00:00Z" };

describe("what you sign in with", () => {
  it("lists every credential with its kind and last use, beside the policy that rules removing one", async () => {
    open("/me", alice());
    const pane = await screen.findByRole("region", { name: "Sign-in methods" });
    expect(await within(pane).findByText("work laptop")).toBeTruthy();
    expect(within(pane).getByText("synced")).toBeTruthy();
    expect(within(pane).getByText("device-bound")).toBeTruthy();
    expect(within(pane).getByText("The password")).toBeTruthy();
    expect(within(pane).getByText("The one-time code generator")).toBeTruthy();
    expect(within(pane).getAllByText("not used yet")).toHaveLength(2);
    expect(within(pane).getByText(/keeps at least 2 passkeys on an account before its password can go/)).toBeTruthy();
  });

  it("removes a passkey only on a second click, and says the API's refusal where the minimum keeps it", async () => {
    const { asked } = open(
      "/me",
      alice({ "DELETE /api/v1/me/credentials/aVBob25lUGFzc2tleQ": { status: 409, body: { error: "removing it would leave fewer passkeys than min_passkeys, 2" } } }),
    );
    const row = (await screen.findByText("phone")).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    expect(screen.getByText("This passkey signs nobody in from now on, and the sessions it opened end.")).toBeTruthy();
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Removing it would leave fewer passkeys than min_passkeys, 2.")).toBeTruthy();
    expect(asked.filter((a) => a.key === "DELETE /api/v1/me/credentials/aVBob25lUGFzc2tleQ")).toHaveLength(1);
  });

  it("removes the generator with a code it shows, which the API checks", async () => {
    const { asked } = open("/me", alice({ "DELETE /api/v1/me/totp": { status: 204 } }));
    const input = await screen.findByLabelText("A code the generator shows now");
    await fireEvent.input(input, { target: { value: "492039" } });
    await fireEvent.submit(input.closest("form")!);
    expect(await screen.findByText("One-time code generator removed.")).toBeTruthy();
    expect(asked.find((a) => a.key === "DELETE /api/v1/me/totp")?.body).toEqual({ totp: "492039" });
  });

  it("adds a passkey with the name given, from the options the API answered", async () => {
    const a = authenticator();
    const { asked } = open(
      "/me",
      alice({
        "POST /api/v1/auth/passkey/options": { status: 200, body: creation },
        "POST /api/v1/auth/passkey/verify": { status: 200, body: { ceremony: "registration", login: "alice", credential: recorded } },
      }),
      { unavailable: "", credentials: a.credentials },
    );
    await fireEvent.input(await screen.findByLabelText(/^Name$/), { target: { value: "desk" } });
    await fireEvent.click(screen.getByRole("button", { name: "Add a passkey" }));
    expect(await screen.findByText("Passkey added.")).toBeTruthy();
    expect(asked.find((x) => x.key === "POST /api/v1/auth/passkey/options")?.body).toEqual({ ceremony: "registration" });
    expect(asked.find((x) => x.key === "POST /api/v1/auth/passkey/verify")?.body).toMatchObject({ ceremony: "registration", label: "desk", credential: { id: "bmV3UGFzc2tleQ" } });
    const publicKey = a.asked.create[0]!.publicKey!;
    expect(Array.from(new Uint8Array(publicKey.user.id as ArrayBuffer))).toEqual(Array.from("alice", (c) => c.charCodeAt(0)));
    expect(publicKey.excludeCredentials).toHaveLength(1);
  });

  it("asks for a sign-in again where the session is too old to add a way in, then adds the passkey", async () => {
    const a = authenticator();
    const stale = { status: 403, body: { error: "adding a way in takes a sign-in in the last 10 minutes" }, headers: { "WWW-Authenticate": 'Bearer error="insufficient_user_authentication", max_age="600"' } };
    const { asked } = open(
      "/me",
      alice({
        "POST /api/v1/auth/passkey/options": [stale, { status: 200, body: { ceremony: "assertion", options: { challenge: "azJNNGVUQTI5VGdScFJQSGE1bXNjdEh5TXFjUTRMNUdEY1NyZlBSTjd1WQ" } } }, { status: 200, body: creation }],
        "POST /api/v1/auth/passkey/verify": [
          { status: 200, body: { ceremony: "assertion", login: "alice" } },
          { status: 200, body: { ceremony: "registration", login: "alice", credential: recorded } },
        ],
      }),
      { unavailable: "", credentials: a.credentials },
    );
    await fireEvent.click(await screen.findByRole("button", { name: "Add a passkey" }));
    expect(await screen.findByText(/Sign in again to add a passkey/)).toBeTruthy();
    expect(screen.getByLabelText("Or with your password")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Sign in again with a passkey" }));
    expect(await screen.findByText("Passkey added.")).toBeTruthy();
    expect(a.asked.get).toHaveLength(1);
    expect(asked.filter((x) => x.key === "POST /api/v1/auth/passkey/options").map((x) => x.body)).toEqual([{ ceremony: "registration" }, { ceremony: "assertion" }, { ceremony: "registration" }]);
    expect(screen.queryByText(/Adding a way in takes a sign-in/)).toBeNull();
  });

  it("says why no passkey can be added in a browser that runs no ceremony", async () => {
    open("/me", alice(), { unavailable: "This browser offers no passkeys on this page." });
    expect(await screen.findByText("This browser offers no passkeys on this page.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add a passkey" })).toBeNull();
  });

  it("draws the signed-out screen once the credential removed ended this session", async () => {
    const s = alice({ "DELETE /api/v1/me/credentials/01M2AAZ9G62NQXFAFCXKRPJEH5": { status: 204 } });
    s["GET /api/v1/me/credentials"] = [s["GET /api/v1/me/credentials"] as Answer, { status: 401, body: { error: "sign in first" } }];
    s["GET /api/v1/me"] = [s["GET /api/v1/me"] as Answer, { status: 401, body: { error: "sign in first" } }];
    open("/me", s);
    const row = (await screen.findByText("The password")).closest("tr")!;
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    await fireEvent.click(within(row).getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("button", { name: "Sign in with a passkey" })).toBeTruthy();
  });
});

describe("API tokens", () => {
  it("lists the tokens with what each is narrowed to, and revokes one on a second click", async () => {
    const { asked } = open("/me/tokens", alice({ "DELETE /api/v1/auth/tokens/01M2AD1R3T5W7Y9A1C3E5G7J9N": { status: 204 } }));
    const row = (await screen.findByRole("cell", { name: "finance/deployer" })).closest("tr")!;
    expect(within(row).getByText("workflow:run within finance/monthly-invoicing")).toBeTruthy();
    expect(screen.getByText("its principal's full rights")).toBeTruthy();
    await fireEvent.click(within(row).getByRole("button", { name: "Revoke" }));
    expect(asked.some((a) => a.key.startsWith("DELETE"))).toBe(false);
    await fireEvent.click(within(row).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText(/^Token revoked\.$/)).toBeTruthy();
    expect(asked.filter((a) => a.key === "DELETE /api/v1/auth/tokens/01M2AD1R3T5W7Y9A1C3E5G7J9N")).toHaveLength(1);
  });

  it("mints a token for a service account, narrowed as asked, and shows its value once", async () => {
    const issued = {
      token: "agktoken_x7Kq2mZr9P4wL1vN8bT3cY6hF0jD5sA2gE9uR4iO7kM",
      api_token: { id: "01M2AD1R3T5W7Y9A1C3E5G7J9P", principal: "finance/deployer", device_label: "deploy pipeline", created_at: "2026-10-01T09:00:00Z", expires_at: "2026-10-31T09:00:00Z" },
    };
    const { asked } = open("/me/tokens", alice({ "POST /api/v1/auth/tokens": { status: 201, body: issued } }));
    const select = (await screen.findByLabelText("For")) as HTMLSelectElement;
    await waitFor(() => expect(within(select).queryByText("finance/deployer")).toBeTruthy());
    expect(within(select).queryByText("finance/agentiik")).toBeNull();
    await fireEvent.change(select, { target: { value: "finance/deployer" } });
    await fireEvent.input(screen.getByLabelText(/^Label$/), { target: { value: "deploy pipeline" } });
    await fireEvent.change(screen.getByLabelText("Expires in"), { target: { value: "30" } });
    await fireEvent.click(screen.getByLabelText("workflow:run"));
    await fireEvent.input(screen.getByLabelText(/^Scope/), { target: { value: "finance/monthly-invoicing, finance/monthly-invoicing" } });
    const before = Date.now();
    await fireEvent.click(screen.getByRole("button", { name: "Mint the token" }));
    expect(await screen.findByText(issued.token)).toBeTruthy();
    expect(screen.getByText(/Shown once: copy it now\./)).toBeTruthy();
    const body = asked.find((a) => a.key === "POST /api/v1/auth/tokens")?.body as Record<string, unknown>;
    expect(body).toMatchObject({ principal: "finance/deployer", device_label: "deploy pipeline", scope: { permissions: ["workflow:run"], within: ["finance/monthly-invoicing"] } });
    const days = (Date.parse(body.expires_at as string) - before) / 86400000;
    expect(Math.round(days)).toBe(30);
    await fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText(issued.token)).toBeNull();
  });

  it("mints one for the caller with its full rights where nothing narrows it", async () => {
    const { asked } = open(
      "/me/tokens",
      alice({ "POST /api/v1/auth/tokens": { status: 201, body: { token: "agktoken_abc", api_token: { id: "01M2AD1R3T5W7Y9A1C3E5G7J9Q", principal: "alice", created_at: "2026-10-01T09:00:00Z", expires_at: "2026-12-30T09:00:00Z" } } } }),
    );
    await fireEvent.click(await screen.findByRole("button", { name: "Mint the token" }));
    expect(await screen.findByText("agktoken_abc")).toBeTruthy();
    const body = asked.find((a) => a.key === "POST /api/v1/auth/tokens")?.body as Record<string, unknown>;
    expect(Object.keys(body)).toEqual(["expires_at"]);
  });

  it("reads a scope and a typed list as the API writes them", () => {
    const t = { id: "x", principal: "alice", created_at: "", expires_at: "", scope: { within: ["finance"] } } as Token;
    expect(scopeOf(t)).toBe("within finance");
    expect(listOf(" finance,team-ops  finance ")).toEqual(["finance", "team-ops"]);
  });
});

describe("the users, for an administrator", () => {
  const users = {
    "GET /api/v1/users": {
      status: 200,
      body: {
        users: [
          { kind: "user", login: "alice", display_name: "Alice Martin", admin: true, suspended: false, created_at: "2026-09-27T08:50:00Z", last_sign_in_at: "2026-09-30T08:00:00Z" },
          { kind: "user", login: "bruno", display_name: "Bruno Petit", admin: false, suspended: true, suspended_for: "no_passkey", created_at: "2026-09-27T08:51:00Z", last_sign_in_at: "2026-09-29T08:00:00Z" },
          { kind: "user", login: "chloe", display_name: "Chloé Durand", admin: false, suspended: false, created_at: "2026-09-30T08:52:00Z" },
        ],
      },
    },
  };

  it("issues a recovery code shown once, and never offers one for the caller's own account", async () => {
    const { asked } = open(
      "/users",
      alice({ ...users, "POST /api/v1/users/bruno/recovery": { status: 201, body: { code: "AB12-CD34-EF56", link: "https://stand-in/auth/enrol#AB12-CD34-EF56", expires_at: "2026-10-01T10:00:00Z" } } }, true),
    );
    const own = (await screen.findByText("Alice Martin", { selector: "td" })).closest("tr")!;
    expect(within(own).getAllByRole("button").map((b) => b.textContent)).toEqual(["Email"]);
    const bruno = screen.getByText("Bruno Petit").closest("tr")!;
    expect(within(bruno).getByText("suspended (no passkey)")).toBeTruthy();
    expect(within(bruno).queryByRole("button", { name: "Enrolment link" })).toBeNull();
    await fireEvent.click(within(bruno).getByRole("button", { name: "Recovery code" }));
    expect(await screen.findByText("https://stand-in/auth/enrol#AB12-CD34-EF56")).toBeTruthy();
    expect(screen.getByText(/^Shown once\. Expires/)).toBeTruthy();
    expect(asked.filter((a) => a.key === "POST /api/v1/users/bruno/recovery")).toHaveLength(1);
  });

  it("offers an enrolment link to a user who never signed in, and says the API's refusal", async () => {
    open("/users", alice({ ...users, "POST /api/v1/users/chloe/enrolment": { status: 409, body: { error: "the user already holds a credential; a lost one is replaced with a recovery code" } } }, true));
    const chloe = (await screen.findByText("Chloé Durand")).closest("tr")!;
    await fireEvent.click(within(chloe).getByRole("button", { name: "Enrolment link" }));
    expect(await screen.findByText("The user already holds a credential; a lost one is replaced with a recovery code.")).toBeTruthy();
  });

  it("is no screen for somebody who is not an administrator", async () => {
    const { asked } = open("/users", alice(users));
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
    expect(asked.some((a) => a.key === "GET /api/v1/users")).toBe(false);
  });
});
