import type { Me, Namespace, Permission } from "../api/client";

// What the caller may do, as GET /api/v1/me gives it: its effective permissions keyed by scope, with
// every deny subtracted and narrowed by the presenting token's scope. The console hides what the
// caller does not hold rather than disabling it: a control nobody may use is noise, and a disabled
// one says that something exists here which the caller cannot reach. The API decides every request
// all the same; this only decides what is drawn.

// holds says whether the caller holds a permission on a namespace, or on one workflow of it. A
// workflow's own key, where /me carries one, holds the whole of what applies there; where it carries
// none, the namespace's applies.
export function holds(me: Me, permission: Permission, namespace: string, workflow?: string): boolean {
  const own = workflow === undefined ? undefined : me.permissions[`${namespace}/${workflow}`];
  return (own ?? me.permissions[namespace] ?? []).includes(permission);
}

// holdsSomewhereIn says whether the caller holds a permission on a namespace or on any one workflow
// of it: run:read granted on one workflow alone opens the namespace's runs, listing that workflow's.
export function holdsSomewhereIn(me: Me, permission: Permission, namespace: string): boolean {
  if (holds(me, permission, namespace)) {
    return true;
  }
  const prefix = `${namespace}/`;
  return Object.entries(me.permissions).some(([scope, held]) => scope.startsWith(prefix) && held.includes(permission));
}

// inNamespace says whether the caller holds anything in a namespace at all, which is what reading it
// takes: its settings, its quotas and its load against them.
export function inNamespace(me: Me, namespace: string): boolean {
  const prefix = `${namespace}/`;
  return Object.entries(me.permissions).some(([scope, held]) => (scope === namespace || scope.startsWith(prefix)) && held.length > 0);
}

// ordered is the namespaces as the switcher lists them: the caller's personal namespace first, the
// other personal ones an administrator sees after it by name, then the shared ones by name. The
// documentation writes "the personal namespace first, shared ones grouped after it".
export function ordered(namespaces: readonly Namespace[], principal: string): { own?: Namespace; personal: Namespace[]; shared: Namespace[] } {
  const byName = (a: Namespace, b: Namespace) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
  const own = namespaces.find((n) => n.kind === "personal" && n.name === principal);
  return {
    own,
    personal: namespaces.filter((n) => n.kind === "personal" && n !== own).sort(byName),
    shared: namespaces.filter((n) => n.kind === "shared").sort(byName),
  };
}

// home is the namespace the console opens on: the caller's personal one, or for a service account the
// namespace it belongs to, or else the first it can read.
export function home(me: Me, namespaces: readonly Namespace[]): string | undefined {
  const { own, personal, shared } = ordered(namespaces, me.principal);
  if (own) {
    return own.name;
  }
  if (me.service_account) {
    return me.service_account.namespace;
  }
  return (shared[0] ?? personal[0])?.name;
}
