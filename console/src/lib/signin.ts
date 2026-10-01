import { Refusal, refusal, type API } from "../api/client";
import type { components } from "../api/schema";
import { explain, Told, type Explained } from "./problem";

// The credential as openapi.json types it: an object whose members WebAuthn defines, which the
// document leaves to that specification rather than repeating it.
type CredentialJSON = components["schemas"]["passkeyVerifyRequest"]["credential"];

// Signing in from the console itself, on the API's origin, which is the Relying Party's: with a
// passkey through POST /api/v1/auth/passkey/options and /verify, as the sign-in page does, or with a
// password through POST /api/v1/auth/login where the policy that applies to the account allows one.
// Either answers with a session cookie beside it, which the console then holds as every request's.
//
// Enrolling stays on the sign-in page's enrol, /auth/enrol, which reads an enrolment link's code
// from after its #, where no address of the console carries one.

// passkeysUnavailable says why no passkey ceremony can run in this browser, and nothing where one
// can: a browser runs one only in a secure context, a page served over https with a certificate it
// trusts, or on localhost.
export function passkeysUnavailable(w: { isSecureContext: boolean; PublicKeyCredential?: unknown; navigator: { credentials?: unknown } }): string {
  if (!w.isSecureContext || !w.PublicKeyCredential || !w.navigator.credentials) {
    return "Passkeys need HTTPS. Sign in with a password.";
  }
  return "";
}

// ceremonyProblem says what a browser's refusal of a ceremony means to the person at it.
export function ceremonyProblem(e: unknown): string {
  const name = e instanceof Error || (typeof e === "object" && e !== null && "name" in e) ? (e as { name?: string }).name : undefined;
  switch (name) {
    case "NotAllowedError":
    case "AbortError":
      return "Cancelled or timed out.";
    case "SecurityError":
      return "The browser refused the passkey on this page.";
  }
  return `The browser could not use the passkey: ${e instanceof Error ? e.message : String(e)}`;
}

// A sign-in the API refused because the policy that applies to the account forbids passwords, which
// it says by naming the setting rather than by failing the password: the form is then withdrawn, and
// never answered as a wrong password.
export class PasswordsForbidden extends Refusal {}

// Too many password sign-ins, with how long Retry-After asks to wait, where it says.
export class TooManyAttempts extends Refusal {
  constructor(
    message: string,
    readonly after: number | null,
  ) {
    super(429, message);
  }
}

// A passkey ceremony the browser ended, told by ceremonyProblem.
export class CeremonyFailed extends Told {}

// explainSignIn tells why a sign-in failed. A 401 here is a sign-in that did not match, never a
// session that ended, which explain would take it for; and a ceremony the browser ended is the
// browser's to say.
export function explainSignIn(how: "passkey" | "password", e: unknown): Explained {
  const failed = how === "passkey" ? "sign in with your passkey" : "sign in with a password";
  const told = explain(failed, e);
  if (e instanceof PasswordsForbidden) return { ...told, why: "Passwords are not allowed for your account.", next: "Use a passkey.", transient: false };
  if (e instanceof TooManyAttempts) return { ...told, why: "Too many attempts.", next: wait(e.after).trim() || "Try again later.", transient: false };
  if (e instanceof Refusal && e.status === 401) {
    return how === "passkey"
      ? { ...told, why: "This passkey was not accepted.", next: "Try again.", transient: false }
      : { ...told, why: "Wrong login, password or code.", next: "", transient: false };
  }
  return told;
}

// signInWithPasskey runs a sign-in ceremony and answers the login it signed in.
export async function signInWithPasskey(api: API, credentials: CredentialsContainer): Promise<string> {
  const started = await api.POST("/api/v1/auth/passkey/options", { body: { ceremony: "assertion" } });
  if (!started.data) {
    throw unavailableOr(refusal(started.response, started.error));
  }
  let credential: Credential | null;
  try {
    credential = await credentials.get({ publicKey: requestOptions(started.data.options as unknown as RequestOptionsJSON) });
  } catch (e) {
    throw new CeremonyFailed(ceremonyProblem(e));
  }
  if (!credential || credential.type !== "public-key") {
    throw new CeremonyFailed("No passkey was returned.");
  }
  const verified = await api.POST("/api/v1/auth/passkey/verify", {
    body: { ceremony: "assertion", credential: credentialJSON(credential as PublicKeyCredential) as CredentialJSON },
  });
  if (!verified.data) {
    throw refusal(verified.response, verified.error);
  }
  return verified.data.login;
}

// signInWithPassword signs in with a login, a password and, where the account holds a generator, a
// TOTP code, and answers who signed in and whether the session may only enrol a passkey.
export async function signInWithPassword(api: API, login: string, password: string, totp: string): Promise<{ login: string; session: string }> {
  const body: { login: string; password: string; totp?: string } = { login, password };
  if (totp !== "") {
    body.totp = totp;
  }
  const { data, error, response } = await api.POST("/api/v1/auth/login", { body });
  if (data) {
    return data;
  }
  const refused = refusal(response, error);
  if (response.status === 403 && typeof error === "object" && error !== null && "setting" in error && error.setting === "password") {
    throw new PasswordsForbidden(refused.status, refused.message);
  }
  if (response.status === 429) {
    const after = Number.parseInt(response.headers.get("Retry-After") ?? "", 10);
    throw new TooManyAttempts(refused.message, Number.isFinite(after) ? after : null);
  }
  throw refused;
}

// unavailableOr is the refusal of an installation addressed by an IP address, which a browser refuses
// as a Relying Party, said for a person, or the refusal as the API gave it.
function unavailableOr(r: Refusal): Refusal {
  if (r.status === 409) {
    return new Refusal(409, "Passkeys need a domain name, not an IP address. Sign in with a password.");
  }
  return r;
}

// sentence is one of the API's sentences as the console shows it, with a capital and a full stop.
export function sentence(text: string): string {
  const t = text.trim();
  if (t === "") {
    return t;
  }
  const capital = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}

// wait says, for a person, how long Retry-After asks them to wait, in whole minutes rounded up, or
// seconds under a minute: the API's sentence speaks of the header, which a person never sees.
export function wait(seconds: number | null): string {
  if (seconds === null || seconds <= 0) {
    return "";
  }
  if (seconds < 60) {
    return ` Try again in ${seconds} ${seconds === 1 ? "second" : "seconds"}.`;
  }
  const minutes = Math.ceil(seconds / 60);
  return ` Try again in ${minutes} ${minutes === 1 ? "minute" : "minutes"}.`;
}

// What WebAuthn Level 3 writes as JSON, and the browser's own conversions where it has them, which
// the ones below stand in for where it does not: the same conversions as the sign-in page's
// codec.js, for a sign-in and for a passkey added from a session.

export interface RequestOptionsJSON {
  challenge: string;
  timeout?: number;
  rpId?: string;
  allowCredentials?: { type: "public-key"; id: string; transports?: AuthenticatorTransport[] }[];
  userVerification?: UserVerificationRequirement;
}

type WithJSON = typeof PublicKeyCredential & { parseRequestOptionsFromJSON?: (json: RequestOptionsJSON) => PublicKeyCredentialRequestOptions };

// requestOptions is parseRequestOptionsFromJSON(): the browser's where it has one, else the options
// with their bytes decoded and every other member as the API wrote it.
export function requestOptions(json: RequestOptionsJSON): PublicKeyCredentialRequestOptions {
  const own = (globalThis as unknown as { PublicKeyCredential?: WithJSON }).PublicKeyCredential;
  if (own && typeof own.parseRequestOptionsFromJSON === "function") {
    return own.parseRequestOptionsFromJSON(json);
  }
  return {
    ...json,
    challenge: decode(json.challenge),
    allowCredentials: (json.allowCredentials ?? []).map((d) => ({ ...d, id: decode(d.id) })),
  };
}

export interface CreationOptionsJSON {
  rp: PublicKeyCredentialRpEntity;
  user: { id: string; name: string; displayName: string };
  challenge: string;
  pubKeyCredParams: PublicKeyCredentialParameters[];
  timeout?: number;
  excludeCredentials?: { type: "public-key"; id: string; transports?: AuthenticatorTransport[] }[];
  authenticatorSelection?: AuthenticatorSelectionCriteria;
  attestation?: AttestationConveyancePreference;
}

type WithCreationJSON = typeof PublicKeyCredential & { parseCreationOptionsFromJSON?: (json: CreationOptionsJSON) => PublicKeyCredentialCreationOptions };

// creationOptions is parseCreationOptionsFromJSON(): the browser's where it has one, else a
// registration's options with their bytes decoded and every other member as the API wrote it.
export function creationOptions(json: CreationOptionsJSON): PublicKeyCredentialCreationOptions {
  const own = (globalThis as unknown as { PublicKeyCredential?: WithCreationJSON }).PublicKeyCredential;
  if (own && typeof own.parseCreationOptionsFromJSON === "function") {
    return own.parseCreationOptionsFromJSON(json);
  }
  return {
    ...json,
    challenge: decode(json.challenge),
    user: { ...json.user, id: decode(json.user.id) },
    excludeCredentials: (json.excludeCredentials ?? []).map((d) => ({ ...d, id: decode(d.id) })),
  };
}

// credentialJSON is toJSON(): the browser's where it has one and it answers, else a
// RegistrationResponseJSON for what create() answered, or an AuthenticationResponseJSON for what
// get() answered, written here. The extensions' results are left empty, since the API asks for none
// and reads none.
export function credentialJSON(credential: PublicKeyCredential): Record<string, unknown> {
  const own = credential as PublicKeyCredential & { toJSON?: () => Record<string, unknown> };
  if (typeof own.toJSON === "function") {
    try {
      return own.toJSON();
    } catch {
      // written below, as the page does for the stand-in some password managers answer with
    }
  }
  const response: Record<string, unknown> = { clientDataJSON: encode(credential.response.clientDataJSON) };
  if ("attestationObject" in credential.response) {
    // Each getter where the browser has it, as codec.js reads them: an older one answers the
    // attestation object alone, which holds the rest.
    const r = credential.response as Partial<AuthenticatorAttestationResponse> & { attestationObject: ArrayBuffer };
    if (typeof r.getAuthenticatorData === "function") {
      response.authenticatorData = encode(r.getAuthenticatorData());
    }
    if (typeof r.getTransports === "function") {
      response.transports = r.getTransports();
    }
    if (typeof r.getPublicKey === "function") {
      const key = r.getPublicKey();
      if (key) {
        response.publicKey = encode(key);
      }
    }
    if (typeof r.getPublicKeyAlgorithm === "function") {
      response.publicKeyAlgorithm = r.getPublicKeyAlgorithm();
    }
    response.attestationObject = encode(r.attestationObject);
  } else {
    const r = credential.response as AuthenticatorAssertionResponse;
    response.authenticatorData = encode(r.authenticatorData);
    response.signature = encode(r.signature);
    if (r.userHandle) {
      response.userHandle = encode(r.userHandle);
    }
  }
  const json: Record<string, unknown> = { id: credential.id, rawId: encode(credential.rawId), type: credential.type, response };
  if (credential.authenticatorAttachment) {
    json.authenticatorAttachment = credential.authenticatorAttachment;
  }
  json.clientExtensionResults = {};
  return json;
}

// How WebAuthn's JSON writes bytes: base64url, with no padding.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
const value = new Map(Array.from(alphabet, (c, i) => [c, i]));

function bytesOf(source: BufferSource): Uint8Array {
  if (source instanceof ArrayBuffer) {
    return new Uint8Array(source);
  }
  return new Uint8Array(source.buffer, source.byteOffset, source.byteLength);
}

// encode writes bytes as base64url with no padding.
export function encode(source: BufferSource): string {
  const b = bytesOf(source);
  let out = "";
  for (let i = 0; i < b.length; i += 3) {
    const n = ((b[i] ?? 0) << 16) | ((b[i + 1] ?? 0) << 8) | (b[i + 2] ?? 0);
    out += alphabet.charAt((n >> 18) & 63) + alphabet.charAt((n >> 12) & 63);
    if (i + 1 < b.length) {
      out += alphabet.charAt((n >> 6) & 63);
    }
    if (i + 2 < b.length) {
      out += alphabet.charAt(n & 63);
    }
  }
  return out;
}

// decode reads base64url with no padding, in the one spelling the API writes: padding, a character
// of another alphabet, a length no bytes encode to and a bit set past the last byte are refused, as
// Go's base64.RawURLEncoding.Strict() refuses them, so that the console and the API never read one
// string as two different things.
export function decode(text: string): ArrayBuffer {
  if (text.length % 4 === 1) {
    throw new TypeError("not base64url: a string of a length some bytes encode to was expected");
  }
  const out = new Uint8Array(Math.floor((text.length * 3) / 4));
  let n = 0;
  let bits = 0;
  let j = 0;
  for (const c of text) {
    const v = value.get(c);
    if (v === undefined) {
      throw new TypeError("not base64url: it holds a character other than A to Z, a to z, 0 to 9, - and _");
    }
    n = (n << 6) | v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[j++] = (n >> bits) & 255;
      n &= (1 << bits) - 1;
    }
  }
  if (n !== 0) {
    throw new TypeError("not base64url: a bit is set past the last byte, which no encoder writes");
  }
  return out.buffer;
}
