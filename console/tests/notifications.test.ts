import { describe, expect, it } from "vitest";
import { dismissible, said, type Notice } from "../src/lib/notifications";

describe("a notification", () => {
  it("says what happened and who did it, naming what it names as it is written", () => {
    expect(said({ id: "01M2AF6V8X0Z2B4D6F8H0K2M4P", kind: "admin_access_widened", at: "2026-09-27T14:00:00Z", act: "granted", by: "carol", namespace: "finance" })).toBe(
      "carol wrote a grant by the installation's power in finance.",
    );
    expect(said({ id: "01M2AH3S5V7X9Z1B3D5F7H9K1M", kind: "break_glass_recovery", at: "2026-09-27T16:40:00Z", login: "alice" })).toBe(
      "A recovery code was issued to alice from the installation's host.",
    );
  });

  const told = (kind: Notice["kind"], rest: Partial<Notice>): Notice => ({ id: "01M2AK0P2R4T6V8X0Z2B4D6F8H", kind, at: "2026-09-27T17:00:00Z", ...rest });

  it("names who issued a recovery code and what it enrolled, and is not offered to be dismissed", () => {
    const issued = told("recovery_code_issued", { by: "carol" });
    const used = told("recovery_code_used", { by: "carol", credential: "bmV3UGFzc2tleQ" });
    expect(said(issued)).toBe("carol issued you a recovery code.");
    expect(said(used)).toBe(
      "A recovery code carol issued enrolled bmV3UGFzc2tleQ on your account. If that was not you, remove it from your sign-in methods and tell another administrator.",
    );
    expect([issued, used].map(dismissible)).toEqual([false, false]);
    expect(dismissible({ id: "01M2AH3S5V7X9Z1B3D5F7H9K1M", kind: "break_glass_recovery", at: "2026-09-27T16:40:00Z", login: "alice" })).toBe(true);
  });

  // A new login names the one the reader had, the one they were given and who gave it, as the API
  // answers it, and is dismissed as the other kinds but a recovery code's are.
  it("names the login a reader was given in place of the one a later release reserved, and is offered to be dismissed", () => {
    const changed = told("login_changed", { from: "mcp", to: "mc-petit", by: "alice" });
    expect(said(changed)).toBe("alice gave you the login mc-petit in place of mcp, a word a later release reserved. You sign in as mc-petit from now on.");
    expect(said(told("login_changed", { from: "mcp", to: "mc-petit", by: "operator" }))).toBe(
      "operator gave you the login mc-petit in place of mcp, a word a later release reserved. You sign in as mc-petit from now on.",
    );
    expect(said(told("login_changed", { by: "alice" }))).toBe("alice gave you a new login in place of one a later release reserved.");
    expect(dismissible(changed)).toBe(true);
  });
});
