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

export function said(n: Notice): string {
  switch (n.kind) {
    case "admin_access_widened":
      return `${n.by ?? "An administrator"} ${n.act ? (acts[n.act] ?? n.act) : "widened access"} in ${n.namespace ?? "a namespace"}.`;
    case "passkey_counter_refused":
      return "A sign-in with one of your passkeys was refused: its signature counter did not move forward, as a copied authenticator's does.";
    case "break_glass_recovery":
      return `A recovery code was issued to ${n.login ?? "an administrator"} from the installation's host.`;
  }
  return n.kind;
}
