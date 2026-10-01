import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { decode, encode, passkeysUnavailable } from "../src/lib/signin";
import type { Passkeys } from "../src/views/SignIn.svelte";
import { scenario, type Scenario } from "./scenario";

// An installation that answers from a scenario, remembers each request's body, and signs the browser
// in where a sign-in route answers 200: GET /api/v1/me then answers alice, as a session cookie set
// beside the answer would have it.
function installation(s: Scenario, headers: Record<string, Record<string, string>> = {}) {
  const asked: { key: string; body: unknown }[] = [];
  const me = scenario("alice")["GET /api/v1/me"];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const text = request.method === "GET" ? "" : await request.text();
    asked.push({ key, body: text ? JSON.parse(text) : undefined });
    const recorded = s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    if (recorded.status === 200 && (key === "POST /api/v1/auth/login" || key === "POST /api/v1/auth/passkey/verify") && me) {
      s["GET /api/v1/me"] = me;
    }
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), {
      status: recorded.status,
      headers: { "Content-Type": "application/json", ...headers[key] },
    });
  };
  return { asked, fetcher };
}

function open(s: Scenario, passkeys: Passkeys = { unavailable: "" }, headers: Record<string, Record<string, string>> = {}) {
  const { asked, fetcher } = installation(s, headers);
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: "/finance/runs", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys });
  return asked;
}

// A scenario of alice's installation where this browser holds no session yet.
function signedOut(extra: Scenario = {}): Scenario {
  return { ...scenario("alice"), "GET /api/v1/me": { status: 401, body: { error: "sign in first" } }, ...extra };
}

async function fillPassword(login: string, password: string, totp = "") {
  await fireEvent.input(await screen.findByLabelText("Login"), { target: { value: login } });
  await fireEvent.input(screen.getByLabelText("Password"), { target: { value: password } });
  if (totp) {
    await fireEvent.input(screen.getByLabelText(/One-time code/), { target: { value: totp } });
  }
  await fireEvent.submit(screen.getByRole("button", { name: "Sign in with a password" }).closest("form")!);
}

describe("signing in from the console", () => {
  afterEach(cleanup);

  it("signs in with a password and draws the console the address names", async () => {
    const asked = open(signedOut({ "POST /api/v1/auth/login": { status: 200, body: { login: "alice", session: "full" } } }));
    await fillPassword("alice", "correct horse battery staple", "492039");
    expect(await screen.findByRole("button", { name: "You, alice" })).toBeTruthy();
    expect(asked.find((a) => a.key === "POST /api/v1/auth/login")?.body).toEqual({ login: "alice", password: "correct horse battery staple", totp: "492039" });
  });

  it("sends no TOTP code where none was typed, and keeps the form open to try again", async () => {
    const asked = open(signedOut({ "POST /api/v1/auth/login": { status: 401, body: { error: "the login, the password or the code does not match" } } }));
    await fireEvent.click(await screen.findByText("Use a password instead"));
    await fillPassword("alice", "wrong");
    expect(await screen.findByText("The login, the password or the code does not match.")).toBeTruthy();
    expect(asked.find((a) => a.key === "POST /api/v1/auth/login")?.body).toEqual({ login: "alice", password: "wrong" });
    expect(screen.getByLabelText("Login").closest("details")?.open).toBe(true);
    expect((screen.getByLabelText("Login") as HTMLInputElement).value).toBe("alice");
  });

  it("withdraws the password form where the policy forbids passwords, never calling it a wrong password", async () => {
    open(signedOut({ "POST /api/v1/auth/login": { status: 403, body: { error: "passwords are forbidden on this installation", setting: "password" } } }));
    await fillPassword("alice", "correct horse battery staple");
    expect(await screen.findByText("Passwords are forbidden on this installation.")).toBeTruthy();
    expect(screen.queryByLabelText("Password")).toBeNull();
    expect(screen.getByRole("button", { name: "Sign in with a passkey" })).toBeTruthy();
  });

  it("says how long to wait once too many passwords were tried", async () => {
    open(signedOut({ "POST /api/v1/auth/login": { status: 429, body: { error: "too many sign-ins, retry after the time Retry-After gives" } } }), { unavailable: "" }, {
      "POST /api/v1/auth/login": { "Retry-After": "90" },
    });
    await fillPassword("alice", "guess");
    expect(await screen.findByText("Too many sign-ins, retry after the time Retry-After gives. Try again in 2 minutes.")).toBeTruthy();
  });

  it("signs in with a passkey, handing the browser the options and the API the browser's answer", async () => {
    const options = { challenge: "vWKo-D9BHOYmP0WPLFLIT4ChZ1IjBlC28Ct4f-5BEAo", timeout: 300000, rpId: "stand-in", allowCredentials: [], userVerification: "required" };
    const answer = { id: "Q2hyb21lUGFzc2tleTAx", rawId: "Q2hyb21lUGFzc2tleTAx", type: "public-key", response: { clientDataJSON: "e30", authenticatorData: "AA", signature: "AA" }, clientExtensionResults: {} };
    let handed: CredentialRequestOptions | undefined;
    const credentials = {
      async get(o: CredentialRequestOptions) {
        handed = o;
        return { type: "public-key", toJSON: () => answer };
      },
    } as unknown as CredentialsContainer;
    const asked = open(
      signedOut({
        "POST /api/v1/auth/passkey/options": { status: 200, body: { ceremony: "assertion", options } },
        "POST /api/v1/auth/passkey/verify": { status: 200, body: { ceremony: "assertion", login: "alice" } },
      }),
      { unavailable: "", credentials },
    );
    await fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByRole("button", { name: "You, alice" })).toBeTruthy();
    expect(asked.find((a) => a.key === "POST /api/v1/auth/passkey/options")?.body).toEqual({ ceremony: "assertion" });
    expect(asked.find((a) => a.key === "POST /api/v1/auth/passkey/verify")?.body).toEqual({ ceremony: "assertion", credential: answer });
    expect(new Uint8Array(handed?.publicKey?.challenge as ArrayBuffer)).toEqual(new Uint8Array(decode(options.challenge)));
  });

  it("says why a ceremony cancelled in the browser signed nobody in", async () => {
    const credentials = {
      async get() {
        throw new DOMException("The operation either timed out or was not allowed.", "NotAllowedError");
      },
    } as unknown as CredentialsContainer;
    const asked = open(signedOut({ "POST /api/v1/auth/passkey/options": { status: 200, body: { ceremony: "assertion", options: { challenge: "AAAA" } } } }), { unavailable: "", credentials });
    await fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByText("The passkey ceremony was cancelled, or it timed out. Try again when you are ready.")).toBeTruthy();
    expect(asked.some((a) => a.key === "POST /api/v1/auth/passkey/verify")).toBe(false);
  });

  it("offers the password alone on an installation addressed by an IP address", async () => {
    const credentials = { get: async () => null } as unknown as CredentialsContainer;
    open(signedOut({ "POST /api/v1/auth/passkey/options": { status: 409, body: { error: "the installation is addressed by an IP address" } } }), { unavailable: "", credentials });
    await fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByText(/Passkeys are unavailable on this installation/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in with a passkey" })).toBeNull();
    expect(screen.getByLabelText("Password")).toBeTruthy();
  });

  it("offers the password where the browser runs no passkey ceremony, and says why", async () => {
    open(signedOut(), { unavailable: passkeysUnavailable({ isSecureContext: false, navigator: {} }) });
    expect(await screen.findByText(/This browser offers no passkeys on this page/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in with a passkey" })).toBeNull();
    expect(screen.getByLabelText("Password")).toBeTruthy();
  });

  it("sends a password sign-in the policy limits to enrolling to the enrolment page", async () => {
    const s = signedOut({ "POST /api/v1/auth/login": { status: 200, body: { login: "bob-martin", session: "enrolment" } } });
    const { fetcher } = installation(s);
    const enrolOnly: typeof fetch = async (input, init) => {
      const answer = await fetcher(input, init);
      s["GET /api/v1/me"] = { status: 403, body: { error: "enrol a passkey first" } };
      return answer;
    };
    const api = connect("http://stand-in/", enrolOnly);
    const place = new Place({ pathname: "/", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
    render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
    await fillPassword("bob-martin", "correct horse battery staple");
    expect(await screen.findByText("This session may enrol a passkey, and nothing else until one is.")).toBeTruthy();
  });

  it("links to the enrolment page, where a link's code is read", async () => {
    open(signedOut());
    expect((await screen.findByRole("link", { name: "Enrol a passkey" })).getAttribute("href")).toBe("auth/enrol");
  });
});

describe("WebAuthn's base64url", () => {
  it("reads back what it writes, whatever the length", () => {
    for (let n = 0; n < 40; n++) {
      const bytes = Uint8Array.from({ length: n }, (_, i) => (i * 37 + n) & 255);
      expect(new Uint8Array(decode(encode(bytes)))).toEqual(bytes);
    }
    expect(encode(new TextEncoder().encode("Agentiik"))).toBe("QWdlbnRpaWs");
  });

  it("refuses what the API would refuse: padding, another alphabet, a length no bytes make, a stray bit", () => {
    for (const bad of ["QWdlbnRpaWs=", "QWdl+nRpaWs", "Q", "QX"]) {
      expect(() => decode(bad)).toThrow(TypeError);
    }
  });
});
