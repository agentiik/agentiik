import { Refusal, refusal, type API } from "../api/client";
import type { components } from "../api/schema";
import { ceremonyProblem, creationOptions, credentialJSON, type CreationOptionsJSON } from "./signin";

// What a person signs in with and what their scripts do, read and changed from the console: the
// passkeys, the password and the one-time code generator GET /api/v1/me/credentials lists, the API
// tokens of the caller and of the service accounts of the namespaces it owns, and, for an
// administrator, the users and the recovery codes and enrolment links that let one back in.
//
// The API holds every rule: the minimum of passkeys the policy keeps, the sign-in in the last 10
// minutes adding a way in takes, a token's longest life. The console says what it answered rather
// than guessing ahead of it, since a rule it guessed would be one more place to keep in step.

export type Credential = components["schemas"]["credential"];
export type Passkey = components["schemas"]["passkey"];
export type Policy = components["schemas"]["authPolicy"];
export type Token = components["schemas"]["apiToken"];
export type TokenRequest = components["schemas"]["tokenRequest"];
export type Issued = components["schemas"]["issuedToken"];
export type ServiceAccount = components["schemas"]["serviceAccount"];
export type User = components["schemas"]["user"];
export type Recovery = components["schemas"]["recoveryCode"];
export type Enrolment = components["schemas"]["enrolmentLink"];

type CredentialJSON = components["schemas"]["passkeyVerifyRequest"]["credential"];

// SignInAgain is the API refusing to add a way in from a session signed in to more than 10 minutes
// ago, which it says with RFC 9470's challenge rather than in words a page would have to match.
export class SignInAgain extends Refusal {}

// refusedFor is why the API said no, as a SignInAgain where it asked for a recent sign-in.
export function refusedFor(response: Response, error: unknown): Refusal {
  const r = refusal(response, error);
  if (response.status === 403 && /insufficient_user_authentication/.test(response.headers.get("WWW-Authenticate") ?? "")) {
    return new SignInAgain(r.status, r.message);
  }
  return r;
}

export async function credentialsOf(api: API): Promise<Credential[]> {
  const { data, error, response } = await api.GET("/api/v1/me/credentials");
  if (!data) {
    throw refusal(response, error);
  }
  return data.credentials;
}

export async function policyOf(api: API): Promise<Policy> {
  const { data, error, response } = await api.GET("/api/v1/auth/policy");
  if (!data) {
    throw refusal(response, error);
  }
  return data;
}

// removeCredential removes a passkey or the password, the generator going with the password.
export async function removeCredential(api: API, id: string): Promise<void> {
  const { error, response } = await api.DELETE("/api/v1/me/credentials/{id}", { params: { path: { id } } });
  if (response.status !== 204) {
    throw refusal(response, error);
  }
}

// removeGenerator removes the one-time code generator with a code it shows now, which is how the
// API knows the device that holds it is at hand and not only a session left open.
export async function removeGenerator(api: API, code: string): Promise<void> {
  const { error, response } = await api.DELETE("/api/v1/me/totp", { body: { totp: code } });
  if (response.status !== 204) {
    throw refusal(response, error);
  }
}

// addPasskey runs a registration ceremony from the session the console holds, and answers the
// passkey recorded. A session signed in to more than 10 minutes ago is refused with SignInAgain.
export async function addPasskey(api: API, credentials: CredentialsContainer, label: string): Promise<Passkey | undefined> {
  const started = await api.POST("/api/v1/auth/passkey/options", { body: { ceremony: "registration" } });
  if (!started.data) {
    throw refusedFor(started.response, started.error);
  }
  let made: globalThis.Credential | null;
  try {
    made = await credentials.create({ publicKey: creationOptions(started.data.options as unknown as CreationOptionsJSON) });
  } catch (e) {
    throw new Error(ceremonyProblem(e));
  }
  if (!made || made.type !== "public-key") {
    throw new Error("No passkey was made. Try again, on this device or on a phone nearby.");
  }
  const body: { ceremony: "registration"; credential: CredentialJSON; label?: string } = {
    ceremony: "registration",
    credential: credentialJSON(made as PublicKeyCredential) as CredentialJSON,
  };
  if (label.trim() !== "") {
    body.label = label.trim();
  }
  const verified = await api.POST("/api/v1/auth/passkey/verify", { body });
  if (!verified.data) {
    throw refusedFor(verified.response, verified.error);
  }
  return verified.data.credential;
}

// described is one credential as its holder reads it in the list.
export function described(c: Credential): string {
  switch (c.type) {
    case "passkey":
      return c.label ? c.label : "A passkey with no name";
    case "password":
      return "The password";
    case "totp":
      return "The one-time code generator";
  }
}

export async function tokensOf(api: API): Promise<Token[]> {
  const { data, error, response } = await api.GET("/api/v1/auth/tokens");
  if (!data) {
    throw refusal(response, error);
  }
  return data.tokens;
}

// serviceAccountsOf is the service accounts a token may be minted for: those of the namespaces the
// caller owns, the built-in NS/agentiik of each left out, since none is minted for it.
export async function serviceAccountsOf(api: API): Promise<ServiceAccount[]> {
  const { data, error, response } = await api.GET("/api/v1/service-accounts");
  if (!data) {
    throw refusal(response, error);
  }
  return data.service_accounts.filter((a) => a.name !== "agentiik");
}

export async function mint(api: API, ask: TokenRequest): Promise<Issued> {
  const { data, error, response } = await api.POST("/api/v1/auth/tokens", { body: ask });
  if (!data) {
    throw refusal(response, error);
  }
  return data;
}

export async function revoke(api: API, id: string): Promise<void> {
  const { error, response } = await api.DELETE("/api/v1/auth/tokens/{id}", { params: { path: { id } } });
  if (response.status !== 204) {
    throw refusal(response, error);
  }
}

// expiringIn is the instant days from now, as a token's expiry is asked for.
export function expiringIn(days: number, now: Date): string {
  return new Date(now.getTime() + days * 24 * 60 * 60 * 1000).toISOString();
}

// scopeOf says what a token is narrowed to, in the words of its scope, or that it is not.
export function scopeOf(t: Token): string {
  if (!t.scope) {
    return "its principal's full rights";
  }
  const parts: string[] = [];
  if (t.scope.permissions) {
    parts.push(t.scope.permissions.join(", "));
  }
  if (t.scope.within) {
    parts.push(`within ${t.scope.within.join(", ")}`);
  }
  return parts.join(" ");
}

// listOf reads a list a person typed, split at commas and white space, each entry once.
export function listOf(typed: string): string[] {
  return [...new Set(typed.split(/[\s,]+/).filter((s) => s !== ""))];
}

export async function usersOf(api: API): Promise<User[]> {
  const { data, error, response } = await api.GET("/api/v1/users");
  if (!data) {
    throw refusal(response, error);
  }
  return data.users;
}

// recoveryFor issues a recovery code for somebody who lost what signs them in, which revokes the one
// they held open.
export async function recoveryFor(api: API, login: string): Promise<Recovery> {
  const { data, error, response } = await api.POST("/api/v1/users/{login}/recovery", { params: { path: { login } } });
  if (!data) {
    throw refusal(response, error);
  }
  return data;
}

// enrolmentFor issues an enrolment link for a user who holds no credential yet.
export async function enrolmentFor(api: API, login: string): Promise<Enrolment> {
  const { data, error, response } = await api.POST("/api/v1/users/{login}/enrolment", { params: { path: { login } } });
  if (!data) {
    throw refusal(response, error);
  }
  return data;
}
