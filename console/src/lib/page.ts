import type { Route } from "./route";

// A tab of a screen's head: a link to where it leads, or a choice made in place, its count beside it.
export type Tab = { label: string; to?: Route; query?: string; current: boolean; count?: number | string; icon?: string; onclick?: () => void };

// workflowTabs are the tabs of a workflow's screens, in one order on each of them: its graph, its
// files, its runs, its statistics, its sharing where the caller manages it, and its MCP tools last,
// where it declares some, so that a tab one screen lacks moves no other.
export function workflowTabs(
  namespace: string,
  workflow: string,
  tab: string | undefined,
  o: { shares: boolean; mcp: boolean; go: (route: Route, query: URLSearchParams) => void },
): Tab[] {
  const page = (t?: string): Route => ({ kind: "namespace", namespace, view: "workflows", workflow, tab: t });
  const sharing: Tab = {
    label: "Sharing",
    icon: "control-share",
    to: { kind: "namespace", namespace, view: "sharing" },
    query: `?workflow=${encodeURIComponent(workflow)}`,
    current: false,
    onclick: () => o.go({ kind: "namespace", namespace, view: "sharing" }, new URLSearchParams({ workflow })),
  };
  return [
    { label: "Graph", icon: "control-workflows", to: page(), current: tab === undefined || tab === "graph" },
    { label: "Files", icon: "control-open", to: page("files"), current: tab === "files" },
    { label: "Runs", icon: "control-runs", to: page("runs"), current: tab === "runs" },
    { label: "Statistics", icon: "control-statistics", to: page("statistics"), current: tab === "statistics" },
    ...(o.shares ? [sharing] : []),
    ...(o.mcp ? [{ label: "MCP", icon: "trigger-mcp", to: page("mcp"), current: tab === "mcp" }] : []),
  ];
}
