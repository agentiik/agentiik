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

// recordedFor is what a scenario holds for a request: the answer recorded for its whole query, or
// else for a part of it, the one naming the most of the request's parameters, or else for its path,
// so that a scenario can tell apart two requests to one route by the parameter that matters,
// by=hour, while the range a test asks for moves with the clock.
export function recordedFor<T>(s: Record<string, T>, method: string, path: string, search: string): T | undefined {
  const key = `${method} ${path}`;
  const exact = s[`${key}${search}`];
  if (exact !== undefined) return exact;
  const asked = new URLSearchParams(search);
  let best: T | undefined;
  let named = 0;
  for (const [recorded, value] of Object.entries(s)) {
    if (!recorded.startsWith(`${key}?`)) continue;
    const wants = [...new URLSearchParams(recorded.slice(key.length + 1))];
    if (wants.length > named && wants.every(([k, v]) => asked.getAll(k).includes(v))) {
      best = value;
      named = wants.length;
    }
  }
  return best ?? s[key];
}

export function answering(s: Scenario, asked: string[] = []): typeof fetch {
  return async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    asked.push(`${key}${url.search}`);
    const recorded = recordedFor(s, request.method, url.pathname, url.search) ?? { status: 404, body: { error: "no such thing, or not yours" } };
    if (recorded.text !== undefined) {
      return new Response(recorded.text, { status: recorded.status, headers: { "Content-Type": "application/octet-stream" } });
    }
    const body = recorded.body === undefined ? null : JSON.stringify(recorded.body);
    return new Response(body, { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
}
