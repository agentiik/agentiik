import { describe, expect, it } from "vitest";
import { address, read } from "../src/lib/route";

describe("an address", () => {
  const root = "/prefix/";

  it("names the screen it opens, and is written back the same", () => {
    for (const path of ["finance/runs", "finance/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A", "finance/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A/against/01JMZ7Q2R5T8V0X2Z4B6D8F0H2", "finance/workflows/monthly-invoicing/statistics", "finance/sharing", "me/tokens", "runners", "users", "groups", "namespaces"]) {
      expect(address(read(root + path, root))).toBe(path);
    }
  });

  it("opens the runs of a namespace named alone", () => {
    expect(read(root + "finance", root)).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: undefined });
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
