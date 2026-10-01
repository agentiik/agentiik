import { describe, expect, it } from "vitest";
import type { Me, Namespace } from "../src/api/client";
import { holds, holdsSomewhereIn, inNamespace, ordered } from "../src/lib/permissions";

const me = (permissions: Me["permissions"], principal = "alice"): Me => ({ principal, admin: false, groups: [], permissions, notifications: [] });

describe("what the caller holds", () => {
  const alice = me({
    finance: ["workflow:read", "run:read", "run:read_data"],
    "finance/monthly-invoicing": ["workflow:read", "run:read"],
    "team-ops/nightly": ["run:read"],
  });

  it("is a workflow's own key where /me carries one, the whole of what applies there", () => {
    expect(holds(alice, "run:read_data", "finance")).toBe(true);
    expect(holds(alice, "run:read_data", "finance", "ledger-export")).toBe(true);
    expect(holds(alice, "run:read_data", "finance", "monthly-invoicing")).toBe(false);
  });

  it("opens a namespace's runs where one workflow of it alone is readable", () => {
    expect(holdsSomewhereIn(alice, "run:read", "team-ops")).toBe(true);
    expect(holds(alice, "run:read", "team-ops")).toBe(false);
    expect(holdsSomewhereIn(alice, "run:read", "team")).toBe(false);
  });

  it("reads a namespace the caller holds anything in, and no other", () => {
    expect(inNamespace(alice, "team-ops")).toBe(true);
    expect(inNamespace(alice, "payroll")).toBe(false);
  });
});

describe("the namespace switcher", () => {
  const namespaces: Namespace[] = [
    { name: "zeta", kind: "shared" },
    { name: "bob", kind: "personal", owner: "bob" },
    { name: "alice", kind: "personal", owner: "alice" },
    { name: "finance", kind: "shared" },
  ];

  it("lists the caller's own first, the shared ones by name, and other people's last", () => {
    const { own, shared, personal } = ordered(namespaces, "alice");
    expect(own?.name).toBe("alice");
    expect(shared.map((n) => n.name)).toEqual(["finance", "zeta"]);
    expect(personal.map((n) => n.name)).toEqual(["bob"]);
  });
});
