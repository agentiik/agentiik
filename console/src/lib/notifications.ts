import type { Me } from "../api/client";

// What a notification of GET /api/v1/me says, in a sentence, the identifiers it names kept as they are
// written everywhere else.

export type Notice = Me["notifications"][number];

const acts: Record<string, string> = {
  granted: "wrote a grant by the installation's power",
  deny_lifted: "lifted a deny from their own access",
  joined_group: "put a user in a group holding a role",
  left_group: "left a group whose deny applied",
  group_removed: "removed a group whose deny applied",
};

// kept are the kinds the API keeps their 90 days, answering 409 to a dismissal: a recovery code issued
// the caller and that code spent, since whoever spent it signs in as the caller and could otherwise
// dismiss both. A new login is dismissed as the other kinds are: nothing it says lets anybody in.
const kept: ReadonlySet<string> = new Set(["recovery_code_issued", "recovery_code_used"]);

// dismissible says whether the caller may dismiss n, which the list offers only then.
export function dismissible(n: Notice): boolean {
  return !kept.has(n.kind);
}

export function said(n: Notice): string {
  // Read as text, so that a kind a later API tells is said by its name rather than by nothing.
  const kind: string = n.kind;
  switch (kind) {
    case "admin_access_widened":
      return `${n.by ?? "An administrator"} ${n.act ? (acts[n.act] ?? n.act) : "widened access"} in ${n.namespace ?? "a namespace"}.`;
    case "passkey_counter_refused":
      return "A sign-in with one of your passkeys was refused: its signature counter did not move forward, as a copied authenticator's does.";
    case "break_glass_recovery":
      return `A recovery code was issued to ${n.login ?? "an administrator"} from the installation's host.`;
    case "recovery_code_issued":
      return `${n.by ?? "An administrator"} issued you a recovery code.`;
    case "recovery_code_used":
      return `A recovery code ${n.by ?? "an administrator"} issued enrolled ${n.credential ?? "a new sign-in method"} on your account. If that was not you, remove it from your sign-in methods and tell another administrator.`;
    case "login_changed":
      // Both logins or neither, since one without the other names nothing a person signs in as.
      if (n.from && n.to) {
        return `${n.by ?? "An administrator"} gave you the login ${n.to} in place of ${n.from}, a word a later release reserved. You sign in as ${n.to} from now on.`;
      }
      return `${n.by ?? "An administrator"} gave you a new login in place of one a later release reserved.`;
  }
  return n.kind;
}
