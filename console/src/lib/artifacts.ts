import type { components } from "../api/schema";
import { clock } from "./format";

// The files a run's steps published, as the run detail keeps them: live while they may be fetched,
// retired once their retain ran out or their fetches were spent, and kept either way, so that a file
// gone is said to be finished rather than shown as missing.

export type Artifact = components["schemas"]["runArtifact"];

// retention says where a file stands, for a person reading the run.
export function retention(a: Artifact, now: number): string {
  switch (a.status) {
    case "live": {
      const left = a.fetches_left === undefined ? "" : `, ${a.fetches_left} ${a.fetches_left === 1 ? "fetch" : "fetches"} left`;
      return `until ${clock(a.expires_at, now)}${left}`;
    }
    case "expired":
      return `expired ${a.retired_at ? clock(a.retired_at, now) : clock(a.expires_at, now)}, past its retain`;
    case "collected":
      return `collected ${a.retired_at ? clock(a.retired_at, now) : ""}, its fetches spent`.replace(" ,", ",");
  }
}

// address is where GET /api/v1/artifacts/{uri} fetches a file, relative to the console's root: the
// URI as one percent-encoded segment, so that a proxy in front passes its slashes undecoded.
export function address(uri: string): string {
  return `api/v1/artifacts/${encodeURIComponent(uri)}`;
}

// fetchable asks the route about a file before the browser fetches it, with a HEAD, which spends
// nothing, and answers why it cannot be fetched, or nothing where it can. A redirect is not
// followed: it names the object store's presigned URL, which is where the browser goes next.
export async function fetchable(fetcher: typeof fetch, url: string): Promise<string> {
  let answer: Response;
  try {
    answer = await fetcher(url, { method: "HEAD", redirect: "manual", credentials: "same-origin" });
  } catch (e) {
    return `Agentiik did not answer when asked about the file. Check your connection, then try again. (The browser said: ${e instanceof Error ? e.message : String(e)})`;
  }
  if (answer.type === "opaqueredirect" || answer.ok || (answer.status >= 300 && answer.status < 400)) {
    return "";
  }
  switch (answer.status) {
    case 410:
      return "This file is no longer available: the time it was kept for has run out, or it has been downloaded as many times as it may be.";
    case 409:
      return "Every download this file has left is in progress for someone else right now. Try again once one of them finishes.";
    case 404:
      return "This file does not exist, or you do not have access to it.";
    case 503:
      return "This installation has no file storage set up, so it cannot serve files. Tell your administrator.";
  }
  return `The server refused to serve the file (it answered ${answer.status}${answer.statusText ? ` ${answer.statusText}` : ""}).`;
}
