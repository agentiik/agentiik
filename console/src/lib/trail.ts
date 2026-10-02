import { runAt, runsOf, type Route } from "./route";

// Where a screen is, as the trail above it says it and as the browser's tab names it.

export type Step = { label: string; to?: Route; code?: boolean };

const labels: Record<string, string> = { workflows: "Workflows", statistics: "Statistics", sharing: "Sharing", variables: "Variables", settings: "Settings" };
const tabs: Record<string, string> = { runs: "Runs", files: "Files", statistics: "Statistics", mcp: "MCP", graph: "Graph", settings: "Settings", profile: "Profile", credentials: "Sign-in methods", tokens: "API tokens", "service-accounts": "Service accounts", policy: "Sign-in policy" };

// trailOf is the trail of a screen: each step a link but the last, which is where the screen is.
export function trailOf(r: Route): Step[] {
  switch (r.kind) {
    case "landing":
      return [{ label: "Home" }];
    case "account":
      return [{ label: "Your account", to: r.tab ? { kind: "account" } : undefined }, ...(r.tab ? [{ label: tabs[r.tab] ?? r.tab }] : [])];
    case "runners":
      return [{ label: "Runners", to: r.tab ? { kind: "runners" } : undefined }, ...(r.tab ? [{ label: "Statistics" }] : [])];
    case "users":
      return [{ label: "Users" }];
    case "groups":
      return [{ label: "Groups" }];
    case "namespaces":
      return [{ label: "Namespaces" }];
    case "unknown":
      return [{ label: "Nothing here" }];
    case "namespace": {
      const out: Step[] = [{ label: r.namespace, to: { kind: "namespace", namespace: r.namespace, view: "workflows" } }];
      const deeper = r.workflow !== undefined || r.run !== undefined;
      out.push({ label: labels[r.view] ?? r.view, to: deeper ? { kind: "namespace", namespace: r.namespace, view: r.view } : undefined });
      if (r.workflow) {
        // A run is under its workflow's runs, each a link back up but the last.
        const below = r.tab !== undefined || r.run !== undefined;
        out.push({ label: r.workflow, to: below ? { kind: "namespace", namespace: r.namespace, view: "workflows", workflow: r.workflow } : undefined });
        if (r.run) out.push({ label: "Runs", to: runsOf(r.namespace, r.workflow) });
        else if (r.tab) out.push({ label: tabs[r.tab] ?? r.tab });
      }
      if (r.run) {
        out.push({ label: r.run, code: true, to: r.against ? runAt(r.namespace, r.workflow, r.run) : undefined });
        if (r.against) out.push({ label: `against ${r.against}`, code: true });
      }
      return out;
    }
  }
}

// titleOf is what the browser's tab says of a screen: where it is, the nearest first, so that a row
// of tabs reads by what each shows rather than by the product's name, which ends it. The home is the
// console itself, and under a workflow the view's name and a run's Runs are left out, since the
// workflow says where they are.
export function titleOf(r: Route): string {
  if (r.kind === "landing") return "Agentiik";
  let steps = trailOf(r);
  if (r.kind === "namespace" && (r.workflow !== undefined || r.run !== undefined)) steps = steps.filter((s, i) => i !== 1 && !(r.run && s.label === "Runs" && !s.code));
  return [...steps.map((s) => s.label).reverse(), "Agentiik"].join(" · ");
}
