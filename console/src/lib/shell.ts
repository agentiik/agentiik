// What the console's frame keeps of one reader's between two loads, in this browser alone: whether the
// sidebar is folded, and the namespace last opened, which the sidebar keeps listing the views of while
// the home or the account is shown, so that its menu stays where it was. A browser that keeps nothing
// unfolds the sidebar and lists the caller's own namespace.

import type { Me, Namespace } from "../api/client";
import { ordered } from "./permissions";

const foldedKey = "agentiik.sidebar";
const namespaceKey = "agentiik.namespace";

export function folded(storage: Storage | undefined): boolean {
  try {
    return storage?.getItem(foldedKey) === "folded";
  } catch {
    return false;
  }
}

export function fold(storage: Storage | undefined, value: boolean): void {
  try {
    if (value) storage?.setItem(foldedKey, "folded");
    else storage?.removeItem(foldedKey);
  } catch {
    // Kept for this page only.
  }
}

export function lastNamespace(storage: Storage | undefined): string | undefined {
  try {
    return storage?.getItem(namespaceKey) ?? undefined;
  } catch {
    return undefined;
  }
}

export function keepNamespace(storage: Storage | undefined, namespace: string): void {
  try {
    storage?.setItem(namespaceKey, namespace);
  } catch {
    // Kept for this page only.
  }
}

// firstNamespace is the namespace the sidebar lists where none was opened yet: the caller's personal
// one, or for a service account the namespace it belongs to, or else the first it can read.
export function firstNamespace(me: Me, namespaces: readonly Namespace[]): string | undefined {
  const { own, personal, shared } = ordered(namespaces, me.principal);
  if (own) return own.name;
  if (me.service_account) return me.service_account.namespace;
  return (shared[0] ?? personal[0])?.name;
}
