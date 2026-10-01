import { describe, expect, it } from "vitest";

// The console lives in the core, beside the routes it calls, and nothing but this check keeps it to
// the ones the API publishes: a route it called that openapi.json does not describe would be one the
// API answers for the console alone, which the documentation never promised and a client of its own
// could not rely on. The generated client refuses a path the document lacks when the types are
// checked; what it does not see is a path written by hand, an EventSource's or a link's. So every
// address of the API's written in the sources is read here, and each must be a path the document
// describes, or an address outside /api/v1 that the console names without calling, said below.

const sources = import.meta.glob(["../src/**/*.ts", "../src/**/*.svelte", "!../src/api/schema.d.ts"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const vendored = import.meta.glob("../vendor/openapi.json", { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const described = Object.keys((JSON.parse(Object.values(vendored)[0]!) as { paths: Record<string, unknown> }).paths);

// The API's own prefixes, which the console never takes an address under.
const prefixes = ["api", "auth", "hooks", "mcp", "objects"];

// The addresses outside /api/v1 the console writes, and why it may: each is shown or linked to,
// and none is fetched by the console.
const named: Record<string, string> = {
  "/auth/enrol": "the sign-in page's enrolment, which a session that may only enrol is sent to, as a link",
  "/mcp/{}/{}": "a workflow's MCP endpoint, which the MCP panel writes out for a client to call",
};

// addresses are the API's addresses a source writes, as a string or a template, each with its
// expressions as {} and its query left out, beside where it is written.
function addresses(file: string, text: string): { at: string; path: string }[] {
  const out: { at: string; path: string }[] = [];
  const literal = /"([^"\n]*)"|'([^'\n]*)'|`([^`]*)`/g;
  for (const m of text.matchAll(literal)) {
    const body = (m[1] ?? m[2] ?? m[3] ?? "").replace(/\$\{[^}]*\}/g, "{}");
    const path = `/${body.replace(/^\//, "")}`.split("?")[0]!;
    const first = path.split("/")[1]!;
    if (!prefixes.includes(first) || path.split("/").length < 3) continue;
    const line = text.slice(0, m.index).split("\n").length;
    out.push({ at: `${file.replace("../", "")}:${line}`, path: path.replace(/\{[^}]*\}/g, "{}") });
  }
  return out;
}

// matches says whether an address written in the console is a path the document describes, its
// parameters matched whatever they are named.
function matches(path: string, documented: string): boolean {
  return documented.replace(/\{[^}]*\}/g, "{}") === path;
}

describe("the routes the console calls", () => {
  const found = Object.entries(sources).flatMap(([file, text]) => addresses(file, text));

  it("are found in the sources, the generated client's and those written by hand", () => {
    expect(found.some((a) => a.path === "/api/v1/runs/{}")).toBe(true);
    expect(found.some((a) => a.at.includes("LogPane") && a.path === "/api/v1/runs/{}/steps/{}/logs")).toBe(true);
  });

  it("are read whole, no literal of the sources swallowing one, as an apostrophe in a comment could", () => {
    const opened = Object.values(sources).reduce((n, text) => n + [...text.matchAll(/["'`]\/?api\/v1\//g)].length, 0);
    expect(found.filter((a) => a.path.startsWith("/api/v1/"))).toHaveLength(opened);
  });

  it("are every one described by the OpenAPI document, or named and never called", () => {
    const strays = found.filter((a) => (a.path.startsWith("/api/v1/") ? !described.some((d) => matches(a.path, d)) : !(a.path in named)));
    expect(strays.map((a) => `${a.at} ${a.path}`)).toEqual([]);
  });

  it("refuses an address the document does not describe, and one under a prefix of the API's it does not name", () => {
    const written = addresses("x.ts", 'fetch(`api/v1/runs/${id}/secrets`); link("/hooks/finance/rerun"); api.GET("/api/v1/runs/{id}")');
    expect(written.map((a) => a.path)).toEqual(["/api/v1/runs/{}/secrets", "/hooks/finance/rerun", "/api/v1/runs/{}"]);
    expect(described.some((d) => matches("/api/v1/runs/{}/secrets", d))).toBe(false);
    expect(described.some((d) => matches("/api/v1/runs/{}", d))).toBe(true);
    expect("/hooks/finance/rerun" in named).toBe(false);
  });
});
