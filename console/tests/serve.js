// A stand-in for agentiik-api, for the screen tests and for looking at the console without an
// installation: it serves the console's build as the API serves it, with the API's own
// Content-Security-Policy and its <base> rewritten to the path given, and answers the API's routes
// from a scenario of recorded answers. No controller, runner or database is behind it.
//
// A scenario is a JSON file under tests/fixtures mapping "METHOD /path" or "METHOD /path?query" to
// {"status": 200, "body": ...}. A route it does not hold is answered as the API answers what the
// caller may not see: 404, no such thing, or not yours.
//
//     node tests/serve.js tests/fixtures/alice.json [port] [path]

import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));
const dist = join(here, "..", "dist");

// The policy is read from where the API writes it, so that the console is tested under the one it is
// served with rather than a copy that drifts from it.
export function consolePolicy() {
  const source = readFileSync(join(here, "..", "..", "api", "console.go"), "utf8");
  const found = /const consolePolicy = "([^"]+)"/.exec(source);
  if (!found) {
    throw new Error("api/console.go writes no consolePolicy this can read");
  }
  return found[1];
}

const types = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
  ".json": "application/json",
};

const apiRoots = ["/api/", "/auth/", "/hooks/", "/mcp", "/objects/"];

// serve starts the stand-in and answers with its address once it listens. scenario may be changed
// between requests by the caller, which is how a test moves the installation along.
export function serve({ scenario, port = 0, prefix = "/" }) {
  const policy = consolePolicy();
  const root = prefix.endsWith("/") ? prefix : prefix + "/";
  const page = readFileSync(join(dist, "index.html"), "utf8").replace('<base href="/">', `<base href="${root}">`);
  const state = { scenario, asked: [] };

  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://stand-in");
    let path = url.pathname;
    if (!path.startsWith(root) && path + "/" !== root) {
      res.writeHead(404).end();
      return;
    }
    path = "/" + path.slice(root.length);

    if (apiRoots.some((r) => path.startsWith(r))) {
      state.asked.push(`${req.method} ${path}${url.search}`);
      const recorded = state.scenario[`${req.method} ${path}${url.search}`] ?? state.scenario[`${req.method} ${path}`];
      const answer = recorded ?? { status: 404, body: { error: "no such thing, or not yours" } };
      res.writeHead(answer.status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
      res.end(answer.body === undefined ? "" : JSON.stringify(answer.body));
      return;
    }

    const headers = {
      "Content-Security-Policy": policy,
      "Referrer-Policy": "no-referrer",
      "X-Content-Type-Options": "nosniff",
      "Cache-Control": "no-cache",
    };
    const file = normalize(path).replace(/^\/+/, "");
    let body;
    try {
      body = file && !file.startsWith(".") ? readFileSync(join(dist, file)) : null;
    } catch {
      body = null;
    }
    if (body === null) {
      res.writeHead(req.method === "GET" || req.method === "HEAD" ? 200 : 404, { ...headers, "Content-Type": types[".html"] });
      res.end(req.method === "GET" ? page : "");
      return;
    }
    res.writeHead(200, { ...headers, "Content-Type": types[extname(file)] ?? "application/octet-stream" });
    res.end(body);
  });

  return new Promise((resolve) => {
    server.listen(port, "127.0.0.1", () => {
      const { port: bound } = server.address();
      resolve({ url: `http://127.0.0.1:${bound}${root}`, state, close: () => new Promise((done) => server.close(done)) });
    });
  });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const [scenarioPath, port = "4173", prefix = "/"] = process.argv.slice(2);
  if (!scenarioPath) {
    console.error("node tests/serve.js <scenario.json> [port] [path]");
    process.exit(2);
  }
  const scenario = JSON.parse(readFileSync(scenarioPath, "utf8"));
  const { url } = await serve({ scenario, port: Number(port), prefix });
  console.log(`the console, against ${scenarioPath}, at ${url}`);
}
