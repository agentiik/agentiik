import { describe, expect, it } from "vitest";
import { address, read, runAt, runsOf } from "../src/lib/route";

describe("an address", () => {
  const root = "/prefix/";

  it("names the screen it opens, and is written back the same", () => {
    for (const path of ["finance/workflows", "finance/workflows/monthly-invoicing/runs", "finance/workflows/monthly-invoicing/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A", "finance/workflows/monthly-invoicing/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A/against/01JMZ7Q2R5T8V0X2Z4B6D8F0H2", "finance/workflows/monthly-invoicing/statistics", "finance/sharing", "me/tokens", "runners", "users", "groups", "namespaces"]) {
      expect(address(read(root + path, root))).toBe(path);
    }
  });

  it("opens the workflows of a namespace named alone", () => {
    expect(read(root + "finance", root)).toEqual({ kind: "namespace", namespace: "finance", view: "workflows", workflow: undefined, tab: undefined });
  });

  it("puts a run under its workflow, and still reads a run's address of before, its workflow not yet known", () => {
    expect(read(root + "finance/workflows/monthly-invoicing/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A", root)).toEqual(runAt("finance", "monthly-invoicing", "01JMZ8V1P9C4XQ7K2N4D6F8H0A"));
    expect(read(root + "finance/runs", root)).toEqual({ kind: "namespace", namespace: "finance", view: "workflows" });
    const before = read(root + "finance/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A/against/01JMZ7Q2R5T8V0X2Z4B6D8F0H2", root);
    expect(before).toEqual({ kind: "namespace", namespace: "finance", view: "workflows", run: "01JMZ8V1P9C4XQ7K2N4D6F8H0A", against: "01JMZ7Q2R5T8V0X2Z4B6D8F0H2" });
    expect(address(before)).toBe("finance/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A/against/01JMZ7Q2R5T8V0X2Z4B6D8F0H2");
    expect(address(runsOf("finance", "monthly-invoicing"))).toBe("finance/workflows/monthly-invoicing/runs");
  });

  it("is the landing at the console's root", () => {
    expect(read(root, root)).toEqual({ kind: "landing" });
  });

  it("names nothing outside the console's root, or past what a screen reads", () => {
    expect(read("/elsewhere/finance/runs", root).kind).toBe("unknown");
    expect(read(root + "finance/nothing", root).kind).toBe("unknown");
    expect(read(root + "finance/runs/a/b", root).kind).toBe("unknown");
    expect(read(root + "finance/runs/a/against", root).kind).toBe("unknown");
    expect(read(root + "finance/runs/a/against/b/c", root).kind).toBe("unknown");
    expect(read(root + "finance/workflows/w/runs/a/b", root).kind).toBe("unknown");
    expect(read(root + "finance/workflows/w/runs/a/against/b/c", root).kind).toBe("unknown");
    expect(read(root + "finance/%E0%A4%A", root).kind).toBe("unknown");
  });

  it("reads me, runners, users, groups and namespaces as the console's own, since no namespace may be named any of them", () => {
    expect(read(root + "me", root)).toEqual({ kind: "account", tab: undefined });
    expect(read(root + "runners/statistics", root)).toEqual({ kind: "runners", tab: "statistics" });
    expect(read(root + "users", root)).toEqual({ kind: "users" });
    expect(read(root + "users/alice", root).kind).toBe("unknown");
    expect(read(root + "groups", root)).toEqual({ kind: "groups" });
    expect(read(root + "namespaces", root)).toEqual({ kind: "namespaces" });
    expect(read(root + "namespaces/finance", root).kind).toBe("unknown");
  });

  it("escapes what it names", () => {
    expect(address({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "a b" })).toBe("finance/workflows/a%20b");
  });
});
