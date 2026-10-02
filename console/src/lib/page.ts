import type { Me } from "../api/client";
import { holds } from "./permissions";
import type { Route } from "./route";

// A tab of a screen's head: a link to where it leads, or a choice made in place, its count beside it.
export type Tab = { label: string; to?: Route; query?: string; current: boolean; count?: number | string; icon?: string; onclick?: () => void };

// workflowTabs are the tabs of a workflow's screens, in one order on each of them: its graph, its
// files, its runs, its statistics, its MCP tools where it declares some, and its settings last, where
// the caller may change any of them, so that a tab one screen lacks moves no other. Who may do what
// with it is the namespace's sharing panel's, where the workflow is chosen, rather than a tab of the
// workflow's own.
export function workflowTabs(
  namespace: string,
  workflow: string,
  tab: string | undefined,
  o: { mcp: boolean; settles?: boolean },
): Tab[] {
  const page = (t?: string): Route => ({ kind: "namespace", namespace, view: "workflows", workflow, tab: t });
  return [
    { label: "Graph", icon: "control-workflows", to: page(), current: tab === undefined || tab === "graph" },
    { label: "Files", icon: "control-open", to: page("files"), current: tab === "files" },
    { label: "Runs", icon: "control-runs", to: page("runs"), current: tab === "runs" },
    { label: "Statistics", icon: "control-statistics", to: page("statistics"), current: tab === "statistics" },
    ...(o.mcp ? [{ label: "MCP", icon: "trigger-mcp", to: page("mcp"), current: tab === "mcp" }] : []),
    ...(o.settles ? [{ label: "Settings", icon: "control-settings", to: page("settings"), current: tab === "settings" }] : []),
  ];
}

// settles says whether the caller may change any of a workflow's settings, which its Settings tab is
// drawn for: its name under workflow:write, its default branch under grant:manage, and its removal
// under workflow:delete.
export function settles(me: Me, namespace: string, workflow: string): boolean {
  return holds(me, "workflow:write", namespace, workflow) || holds(me, "grant:manage", namespace, workflow) || holds(me, "workflow:delete", namespace, workflow);
}
