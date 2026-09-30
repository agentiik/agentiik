// A fetch answering from a recorded scenario, as tests/serve.js answers a browser: a route the
// scenario does not hold is answered as the API answers what the caller may not see.

export type Scenario = Record<string, { status: number; body?: unknown }>;

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
    const body = recorded.body === undefined ? null : JSON.stringify(recorded.body);
    return new Response(body, { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
}
