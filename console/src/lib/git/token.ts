import type { API } from "../../api/client";
import { mint, revoke } from "../credentials";
import type { Remote } from "./protocol";

// The repository takes an API token and nothing else: it drops the session's cookie before reading
// anything, so that a page a signed-in browser is sent to cannot make it clone or push (Sessions
// and tokens), and answers a request without a token 401 asking for Basic, which a browser meets
// with a box of its own asking for a user name and a password. So a commit from the console pushes
// with a token of its own: minted as the commit starts, narrowed to what a push asks of its pusher
// and to ten minutes, held by this page alone and sent as Bearer, and revoked once the push is
// answered, whatever it answered.

// What a push asks of its pusher: to read the repository and write it, to move a protected default
// branch, and to name a secret, which the hook checks; and, unnarrowed by within, to read a library
// an include names in another namespace.
export const pushing = ["workflow:read", "workflow:write", "grant:manage", "secret:use"] as const;

// lifetime is how long the token outlives the push should the page be closed before revoking it.
export const lifetime = 10 * 60_000;

export async function withPushToken<T>(api: API, url: string, work: (remote: Remote) => Promise<T>, now = Date.now()): Promise<T> {
  const issued = await mint(api, { device_label: "web console commit", expires_at: new Date(now + lifetime).toISOString(), scope: { permissions: [...pushing] } });
  const remote: Remote = {
    url,
    fetch: (input, init) => {
      const headers = new Headers(init?.headers);
      headers.set("Authorization", `Bearer ${issued.token}`);
      return fetch(input, { ...init, headers, credentials: "omit" });
    },
  };
  try {
    return await work(remote);
  } finally {
    await revoke(api, issued.api_token.id).catch(() => undefined);
  }
}
