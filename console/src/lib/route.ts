// The console's addresses. Every address outside the API's own is the console's, and the API answers
// each with the console's page, whose script reads the address and draws what it names: so an address
// is the whole of what a screen shows, and one pasted into another browser opens the same screen.
//
// A namespace is the first segment of most of them, as it is of the API's routes. The others begin
// with a word no namespace can be named, the reserved words of the namespace grammar: me for the
// caller's own account, runners for what an administrator runs, users, groups and namespaces for whom
// and what an administrator manages. A workflow is the segment after workflows, and its runs are
// under it: its runs, then a run as the API names it, and two runs read side by side are the
// first's address, then against and the second. A run is always some workflow's, so it is reached
// from that workflow and nowhere else.
//
// The addresses runs had before, a namespace then runs, are still read: the list as the namespace's
// workflows, and a run as itself with its workflow not yet known, which its screen writes into the
// address once it has read the run.

export type View = "workflows" | "statistics" | "sharing" | "variables" | "settings";

export const views: readonly View[] = ["workflows", "statistics", "sharing", "variables", "settings"];

export type Route =
  | { kind: "landing" }
  | { kind: "namespace"; namespace: string; view: View; workflow?: string; run?: string; against?: string; tab?: string }
  | { kind: "account"; tab?: string }
  | { kind: "runners"; tab?: string }
  | { kind: "users"; tab?: string }
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
  // The users' page has a tab of its own, the installation's sign-in policy, under the users' segment
  // rather than one of its own, since a first segment of the console's is one no namespace may take;
  // and the audit log is under it for the same reason, an entry of the sidebar of its own.
  if (first === "users" && (second === "policy" || second === "audit") && third === undefined) {
    return { kind: "users", tab: second };
  }
  if (first === "users" || first === "groups" || first === "namespaces") {
    return second === undefined ? { kind: first } : { kind: "unknown", path: pathname };
  }
  if (first === "runners") {
    return third === undefined ? { kind: "runners", tab: second } : { kind: "unknown", path: pathname };
  }
  if (second === "runs") {
    if (third === undefined) return { kind: "namespace", namespace: first, view: "workflows" };
    if (fourth === undefined) return { kind: "namespace", namespace: first, view: "workflows", run: third };
    if (fourth === "against" && rest.length === 1) return { kind: "namespace", namespace: first, view: "workflows", run: third, against: rest[0] };
    return { kind: "unknown", path: pathname };
  }
  if (second === "workflows" && third !== undefined && fourth === "runs") {
    const [run, against, other, ...beyond] = rest;
    if (run === undefined) return { kind: "namespace", namespace: first, view: "workflows", workflow: third, tab: "runs" };
    if (against === undefined) return { kind: "namespace", namespace: first, view: "workflows", workflow: third, run };
    if (against === "against" && other !== undefined && beyond.length === 0) return { kind: "namespace", namespace: first, view: "workflows", workflow: third, run, against: other };
    return { kind: "unknown", path: pathname };
  }
  if (rest.length > 0) {
    return { kind: "unknown", path: pathname };
  }
  const view = (second ?? "workflows") as View;
  if (!views.includes(view)) {
    return { kind: "unknown", path: pathname };
  }
  switch (view) {
    case "workflows":
      return { kind: "namespace", namespace: first, view, workflow: third, tab: fourth };
    default:
      return third === undefined ? { kind: "namespace", namespace: first, view } : { kind: "unknown", path: pathname };
  }
}

// runAt is the address of a run, under the workflow it is a run of; and of two runs read side by
// side where against names the second. A run whose workflow is not known yet is addressed as before,
// and its screen writes the workflow in once it has read the run.
export function runAt(namespace: string, workflow: string | undefined, run: string, against?: string): Route {
  return { kind: "namespace", namespace, view: "workflows", ...(workflow ? { workflow } : {}), run, ...(against ? { against } : {}) };
}

// runsOf is the address of a workflow's runs.
export function runsOf(namespace: string, workflow: string): Route {
  return { kind: "namespace", namespace, view: "workflows", workflow, tab: "runs" };
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
      return route.tab ? `users/${e(route.tab)}` : "users";
    case "groups":
    case "namespaces":
      return route.kind;
    case "unknown":
      return "./";
    case "namespace": {
      if (route.run) {
        // A run whose workflow is not known yet keeps the address it was reached by.
        let path = route.workflow ? `${e(route.namespace)}/workflows/${e(route.workflow)}/runs/${e(route.run)}` : `${e(route.namespace)}/runs/${e(route.run)}`;
        if (route.against) {
          path += `/against/${e(route.against)}`;
        }
        return path;
      }
      let path = `${e(route.namespace)}/${route.view}`;
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
