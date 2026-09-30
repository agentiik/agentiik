import createClient, { type Client } from "openapi-fetch";
import type { components, paths } from "./schema";

// The installation's API, as the console reaches it: on the page's own origin, under the path the
// page's <base> names, which the API rewrote to the public URL's path. Every route the console calls
// is one openapi.json describes, since the client is typed from it and a path it does not describe
// does not compile: console and API cannot speak two dialects.
//
// Requests carry the session cookie and nothing else, credentials same-origin, in fetch's default
// mode, cors: the API refuses a request changing something whose Origin is not the public URL's, and
// a browser writes Origin: null on a request of another mode from a page that sends no Referer.

export type API = Client<paths>;

export type Me = components["schemas"]["me"];
export type Namespace = components["schemas"]["namespaceList"]["namespaces"][number];
export type Permission = Me["permissions"][string][number];

// apiBase is where every path of the API is appended: the directory the page's <base> names, with
// no trailing slash, since every path of openapi.json starts with one.
export function apiBase(baseURI: string): string {
  return new URL(".", baseURI).href.replace(/\/$/, "");
}

// connect is the client of the installation the page was served by, or of the one a test hands it
// a fetch for.
export function connect(baseURI: string, fetcher?: typeof fetch): API {
  return createClient<paths>({ baseUrl: apiBase(baseURI), credentials: "same-origin", fetch: fetcher });
}

// Refusal is an answer that says no, with the status and the sentence the API gave, which it writes
// as {"error": ...}.
export class Refusal extends Error {
  constructor(
    readonly status: number,
    said: string,
  ) {
    super(said);
  }
}

// refusal reads why the API said no, and its status line where it wrote nothing that reads.
export function refusal(response: Response, error: unknown): Refusal {
  const said = typeof error === "object" && error !== null && "error" in error && typeof error.error === "string" ? error.error : `${response.status} ${response.statusText}`.trim();
  return new Refusal(response.status, said);
}
