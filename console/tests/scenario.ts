// A fetch answering from a recorded scenario, as tests/serve.js answers a browser: a route the
// scenario does not hold is answered as the API answers what the caller may not see.

// An answer is JSON where it records a body, and the bytes as written where it records text, as a
// file of a repository's tree is answered.
export type Scenario = Record<string, { status: number; body?: unknown; text?: string }>;

const recorded = import.meta.glob("./fixtures/*.json", { eager: true, import: "default" }) as Record<string, Scenario>;

// scenario is a recorded scenario by the name of its file, copied, so that a test changing it changes
// it for itself alone.
export function scenario(name: string): Scenario {
  const s = recorded[`./fixtures/${name}.json`];
  if (!s) {
    throw new Error(`no scenario tests/fixtures/${name}.json`);
  }
  return structuredClone(s);
}

export function answering(s: Scenario, asked: string[] = []): typeof fetch {
  return async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    asked.push(`${key}${url.search}`);
    const recorded = s[`${key}${url.search}`] ?? s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    if (recorded.text !== undefined) {
      return new Response(recorded.text, { status: recorded.status, headers: { "Content-Type": "application/octet-stream" } });
    }
    const body = recorded.body === undefined ? null : JSON.stringify(recorded.body);
    return new Response(body, { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
}
