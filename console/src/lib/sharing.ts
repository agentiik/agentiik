import type { components } from "../api/schema";
import type { Permission } from "../api/client";

// Sharing, as the console reads and explains it: the grants and denies written on a namespace or a
// workflow, as GET .../grants answers them, and what they come to for one principal, worked out
// the way the API's access.Resolve works it out, so that the panel can show the arithmetic the
// API does: "Effective permissions are the union of every applying grant: the principal's own and
// its groups', at both scopes. A workflow-scope grant only adds. Only an explicit deny removes, and
// it wins over any allow at any scope." The API decides every request all the same; this says why.

export type Grant = components["schemas"]["accessGrant"];
export type Role = NonNullable<Grant["role"]>;

// The nine, in the order the documentation's tables name them.
export const permissions: Permission[] = ["workflow:read", "workflow:run", "workflow:write", "workflow:delete", "run:read", "run:read_data", "secret:use", "secret:write", "grant:manage"];

// The roles' columns, as the documentation's table of roles writes them: "A role holds every
// permission of each column it says yes to, so each of the nine sits in one column, run:read in
// two."
export const columns: { name: string; permissions: Permission[] }[] = [
  { name: "read", permissions: ["workflow:read", "run:read"] },
  { name: "run", permissions: ["workflow:run", "run:read"] },
  { name: "write", permissions: ["workflow:write", "secret:use"] },
  { name: "data", permissions: ["run:read_data"] },
  { name: "secret", permissions: ["secret:write"] },
  { name: "delete", permissions: ["workflow:delete"] },
  { name: "grant", permissions: ["grant:manage"] },
];

export const roles: { role: Role; columns: string[] }[] = [
  { role: "viewer", columns: ["read"] },
  { role: "operator", columns: ["run"] },
  { role: "editor", columns: ["read", "run", "write", "data", "secret"] },
  { role: "owner", columns: ["read", "run", "write", "data", "secret", "delete", "grant"] },
];

// carries is what a role holds.
export function carries(role: Role): Set<Permission> {
  const held = roles.find((r) => r.role === role)?.columns ?? [];
  return new Set(columns.filter((c) => held.includes(c.name)).flatMap((c) => c.permissions));
}

// secret:use and secret:write "count at namespace scope only, since a declaration serves every
// workflow of the namespace", so a grant on one workflow never gives them, whatever its role.
const namespaceOnly: Permission[] = ["secret:use", "secret:write"];

// Kind is what a grant's principal names, by how it is written: group:NAME, NS/NAME for a service
// account, a login otherwise.
export type Kind = "user" | "group" | "service account";

export function kindOf(principal: string): Kind {
  if (principal.startsWith("group:")) return "group";
  return principal.includes("/") ? "service account" : "user";
}

// Scope is where a grant is written or a question is asked: a namespace, or one workflow of it.
export type Scope = { namespace: string; workflow?: string };

export function scopeOf(written: string): Scope {
  const [namespace, workflow] = written.split("/");
  return { namespace: namespace!, workflow };
}

// covers says whether a grant written at one scope applies to a question at another: a
// namespace's grants to the namespace and every workflow in it, a workflow's to that workflow alone.
export function covers(written: string, at: Scope): boolean {
  const s = scopeOf(written);
  return s.namespace === at.namespace && (s.workflow === undefined || s.workflow === at.workflow);
}

// expired says whether a grant has ended: "A grant or a deny ends at the instant its expiry names".
export function expired(g: Grant, now: number): boolean {
  return g.expires_at !== undefined && Date.parse(g.expires_at) <= now;
}

// Who is the principal a question is asked about: as grants name it, and its groups where the
// caller can read them, which is what decides whether a group's grants are its own. groups
// undefined says they could not be read: a grant to a group may then apply, and is said to.
export type Who = { ref: string; groups?: string[] };

// Line is one permission of the arithmetic: the grants that give it, the denies that take it, and,
// where the principal's groups could not be read, the grants of groups that would give it to a
// member; held is what the three come to.
export type Line = { permission: Permission; gives: Grant[]; takes: Grant[]; ifMember: Grant[]; held: boolean };

function named(who: Who, principal: string): "own" | "group" | "maybe" | undefined {
  if (principal === who.ref) return "own";
  if (!principal.startsWith("group:")) return undefined;
  if (who.groups === undefined) return kindOf(who.ref) === "group" ? undefined : "maybe";
  return who.groups.includes(principal.slice("group:".length)) ? "group" : undefined;
}

// resolve is what who holds at one scope as of now, permission by permission, with what gives each
// and what takes it away, as access.Resolve answers it.
export function resolve(who: Who, grants: readonly Grant[], at: Scope, now: number): Line[] {
  const applying = grants.filter((g) => covers(g.scope, at) && !expired(g, now));
  return permissions.map((permission) => {
    const gives: Grant[] = [];
    const takes: Grant[] = [];
    const ifMember: Grant[] = [];
    for (const g of applying) {
      const whose = named(who, g.principal);
      if (!whose) continue;
      if (g.deny !== undefined) {
        if (g.deny === permission && whose !== "maybe") takes.push(g);
        continue;
      }
      if (g.role === undefined || !carries(g.role).has(permission)) continue;
      if (scopeOf(g.scope).workflow !== undefined && namespaceOnly.includes(permission)) continue;
      (whose === "maybe" ? ifMember : gives).push(g);
    }
    return { permission, gives, takes, ifMember, held: gives.length > 0 && takes.length === 0 };
  });
}

// principalsOf is every principal the grants name, users first, then groups, then service
// accounts, each by name: whom the panel offers to resolve.
export function principalsOf(grants: readonly Grant[]): string[] {
  const order: Kind[] = ["user", "group", "service account"];
  return [...new Set(grants.map((g) => g.principal))].sort((a, b) => order.indexOf(kindOf(a)) - order.indexOf(kindOf(b)) || (a < b ? -1 : a > b ? 1 : 0));
}

// groupsOf reads a principal's groups from what GET /api/v1/groups answers an administrator, by
// name, the way a grant names them after group:.
export function groupsOf(login: string, groups: readonly { name: string; members: string[] }[]): string[] {
  return groups.filter((g) => g.members.includes(login)).map((g) => g.name);
}

// expiryOf is an expiry the add control takes, in days from now or as a date, as an instant the API
// reads, or nothing for a grant that lasts until it is revoked.
export function expiryOf(written: string, now: number): string | undefined | Error {
  const text = written.trim();
  if (text === "") return undefined;
  const days = /^(\d+)\s*d(ays?)?$/.exec(text);
  if (days) {
    const n = Number(days[1]);
    return n > 0 ? new Date(now + n * 86_400_000).toISOString() : new Error("an expiry is at least a day away");
  }
  const at = Date.parse(/^\d{4}-\d{2}-\d{2}$/.test(text) ? `${text}T00:00:00Z` : text);
  if (Number.isNaN(at)) return new Error("not a number of days, a date or an instant");
  if (at <= now) return new Error("that instant has passed");
  return new Date(at).toISOString();
}
