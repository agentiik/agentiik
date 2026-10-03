import { describe, expect, it } from "vitest";
import type { Graph } from "../src/lib/graph";
import { toolOf } from "../src/lib/tool";

// The tool a version publishes, as a client is told of it: an async tool answers its run and never
// the output, so it publishes no outputSchema, which a structured result would have to conform to.
describe("the tool a version publishes", () => {
  const graph = (mode?: "sync" | "async") =>
    ({
      inputs: { orders: { schema: { type: "array" }, required: true } },
      outputs: { invoices: { from: { step: "archive", port: "ok" }, schema: { type: "object" } } },
      mcp: { description: "Issue the invoices.", output: "invoices", ...(mode ? { mode } : {}) },
    }) as unknown as Graph;

  it("publishes the output's schema where it waits for its run", () => {
    expect(toolOf(graph(), "monthly-invoicing")?.outputSchema).toEqual({ type: "object" });
    expect(toolOf(graph("sync"), "monthly-invoicing")?.outputSchema).toEqual({ type: "object" });
  });

  it("publishes none where it answers the run it started", () => {
    const tool = toolOf(graph("async"), "monthly-invoicing");
    expect(tool?.output).toBe("invoices");
    expect(tool?.outputSchema).toBeUndefined();
  });
});
