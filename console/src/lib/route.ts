// The console's addresses. Every address outside the API's own is the console's, and the API answers
// each with the console's page, whose script reads the address and draws what it names: so an address
// is the whole of what a screen shows, and one pasted into another browser opens the same screen.
//
// A namespace is the first segment of most of them, as it is of the API's routes. The others begin
// with a word no namespace can be named, the reserved words of the namespace grammar: me for the
// caller's own account, runners for what an administrator runs, users, groups and namespaces for whom
// and what an administrator manages. A workflow is the segment after
// workflows, and a run the segment after runs, each as the API names it; two runs read side by side
// are the first's address, then against and the second.

export type View = "runs" | "workflows" | "statistics" | "sharing" | "settings";

export const views: readonly View[] = ["runs", "workflows", "statistics", "sharing", "settings"];

export type Route =
  | { kind: "landing" }
  | { kind: "namespace"; namespace: string; view: View; workflow?: string; run?: string; against?: string; tab?: string }
  | { kind: "account"; tab?: string }
  | { kind: "runners"; tab?: string }
  | { kind: "users" }
  | { kind: "groups" }
  | { kind: "namespaces" }
  | { kind: "unknown"; path: string };

// segmentsOf is the path of an address after the console's root, the <base> the page was served with,
// cut into its segments and each decoded once.
export function segmentsOf(pathname: string, root: string): string[] | null {
  if (!pathname.startsWith(root)) {
    return null;
  }
  try {
    return pathname
      .slice(root.length)
      .split("/")
      .filter((s) => s !== "")
      .map((s) => decodeURIComponent(s));
  } catch {
    return null;
  }
}

// read is the route an address names, relative to the console's root.
export function read(pathname: string, root: string): Route {
  const segments = segmentsOf(pathname, root);
  if (segments === null) {
    return { kind: "unknown", path: pathname };
  }
  const [first, second, third, fourth, ...rest] = segments;
  if (first === undefined) {
    return { kind: "landing" };
  }
  if (first === "me") {
    return third === undefined ? { kind: "account", tab: second } : { kind: "unknown", path: pathname };
  }
  if (first === "users" || first === "groups" || first === "namespaces") {
    return second === undefined ? { kind: first } : { kind: "unknown", path: pathname };
  }
  if (first === "runners") {
    return third === undefined ? { kind: "runners", tab: second } : { kind: "unknown", path: pathname };
  }
  if (second === "runs" && third !== undefined && fourth === "against" && rest.length === 1) {
    return { kind: "namespace", namespace: first, view: "runs", run: third, against: rest[0] };
  }
  if (rest.length > 0) {
    return { kind: "unknown", path: pathname };
  }
  const view = (second ?? "runs") as View;
  if (!views.includes(view)) {
    return { kind: "unknown", path: pathname };
  }
  switch (view) {
    case "runs":
      return fourth === undefined ? { kind: "namespace", namespace: first, view, run: third } : { kind: "unknown", path: pathname };
    case "workflows":
      return { kind: "namespace", namespace: first, view, workflow: third, tab: fourth };
    default:
      return third === undefined ? { kind: "namespace", namespace: first, view } : { kind: "unknown", path: pathname };
  }
}

// address is the path of a route relative to the console's root, which a link writes as it is, since
// the page's <base> resolves it.
export function address(route: Route): string {
  const e = encodeURIComponent;
  switch (route.kind) {
    case "landing":
      return "./";
    case "account":
      return route.tab ? `me/${e(route.tab)}` : "me";
    case "runners":
      return route.tab ? `runners/${e(route.tab)}` : "runners";
    case "users":
    case "groups":
    case "namespaces":
      return route.kind;
    case "unknown":
      return "./";
    case "namespace": {
      let path = `${e(route.namespace)}/${route.view}`;
      if (route.view === "runs" && route.run) {
        path += `/${e(route.run)}`;
        if (route.against) {
          path += `/against/${e(route.against)}`;
        }
      }
      if (route.view === "workflows" && route.workflow) {
        path += `/${e(route.workflow)}`;
        if (route.tab) {
          path += `/${e(route.tab)}`;
        }
      }
      return path;
    }
  }
}

// rootOf is the path the console is served under, as the page's <base> names it, ending in a slash.
export function rootOf(baseURI: string): string {
  return new URL(".", baseURI).pathname;
}
