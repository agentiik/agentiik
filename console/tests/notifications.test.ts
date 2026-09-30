import { describe, expect, it } from "vitest";
import { said } from "../src/lib/notifications";

describe("a notification", () => {
  it("says what happened and who did it, naming what it names as it is written", () => {
    expect(said({ id: "01M2AF6V8X0Z2B4D6F8H0K2M4P", kind: "admin_access_widened", at: "2026-09-27T14:00:00Z", act: "granted", by: "carol", namespace: "finance" })).toBe(
      "carol wrote a grant by the installation's power in finance.",
    );
    expect(said({ id: "01M2AH3S5V7X9Z1B3D5F7H9K1M", kind: "break_glass_recovery", at: "2026-09-27T16:40:00Z", login: "alice" })).toBe(
      "A recovery code was issued to alice from the installation's host.",
    );
  });
});
