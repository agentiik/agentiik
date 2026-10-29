import { describe, expect, it } from "vitest";
import type { Graph } from "../src/lib/graph";
import { toolOf } from "../src/lib/tool";

// The tool a version publishes, as a client is told of it: its inputSchema the object of the
// workflow's inputs and its outputSchema the envelope of the output it returns, each item's data held
// to the output's schema, since the protocol holds a structured result to its outputSchema and the
// result is the envelope. An async tool answers its run and never the output, so it publishes none.
describe("the tool a version publishes", () => {
  const graph = (mode?: "sync" | "async") =>
    ({
      inputs: { orders: { schema: { type: "array" }, required: true } },
      outputs: { invoices: { from: { step: "archive", port: "ok" }, schema: { type: "object" } } },
      mcp: { description: "Issue the invoices.", output: "invoices", ...(mode ? { mode } : {}) },
    }) as unknown as Graph;

  it("publishes the envelope of the output where it waits for its run, its items' data held to the output's schema", () => {
    for (const tool of [toolOf(graph(), "monthly-invoicing"), toolOf(graph("sync"), "monthly-invoicing")]) {
      const envelope = tool?.outputSchema as { required: string[]; properties: { meta: unknown; items: { items: { required: string[]; properties: { data: unknown } } } } };
      expect(envelope.required).toEqual(["meta", "items"]);
      expect(envelope.properties.meta).toEqual({ type: "object" });
      expect(envelope.properties.items.items.required).toEqual(["id", "data", "files"]);
      expect(envelope.properties.items.items.properties.data).toEqual({ type: "object" });
    }
  });

  it("publishes none where it answers the run it started, or where the output carries no schema", () => {
    const tool = toolOf(graph("async"), "monthly-invoicing");
    expect(tool?.output).toBe("invoices");
    expect(tool?.outputSchema).toBeUndefined();
    const plain = graph();
    (plain.outputs as Record<string, { schema?: unknown }>).invoices!.schema = undefined;
    expect(toolOf(plain, "monthly-invoicing")?.outputSchema).toBeUndefined();
  });

  // What api/collection_mcp.go's bundled writes for the same schemas, read off it: a client is given
  // these very bytes, every reference a pointer from the root, no $id, $anchor or embedded $schema
  // left, and every object's keys in order.
  it("publishes each schema as the server writes it", () => {
    const tool = toolOf(
      {
        inputs: {
          orders: { schema: { $schema: "http://json-schema.org/draft-07/schema#", $id: "https://example.com/orders", type: "array", items: { $ref: "#/$defs/order" }, $defs: { order: { $anchor: "order", type: "object", properties: { n: { $ref: "#order" } } } }, examples: [{ $ref: "kept" }] }, required: true },
          note: { schema: { $ref: "#line", $defs: { line: { $anchor: "line", type: "string" } } } },
          "a b": { schema: { $ref: "#/$defs/x y", $defs: { "x y": { type: "string" } } }, required: true, default: "x" },
          plain: {},
        },
        outputs: { invoices: { from: { step: "archive", port: "ok" }, schema: { type: "array", items: { $ref: "#/$defs/i" }, $defs: { i: { type: "object" } } } } },
        mcp: { description: "Issue the invoices.", output: "invoices" },
      } as unknown as Graph,
      "monthly-invoicing",
    )!;
    expect(JSON.stringify(tool.inputSchema)).toBe("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"additionalProperties\":false,\"properties\":{\"a b\":{\"$defs\":{\"x y\":{\"type\":\"string\"}},\"$ref\":\"#/properties/a%20b/$defs/x%20y\"},\"note\":{\"$defs\":{\"line\":{\"type\":\"string\"}},\"$ref\":\"#/properties/note/$defs/line\"},\"orders\":{\"$defs\":{\"order\":{\"properties\":{\"n\":{\"$ref\":\"#/properties/orders/$defs/order\"}},\"type\":\"object\"}},\"examples\":[{\"$ref\":\"kept\"}],\"items\":{\"$ref\":\"#/properties/orders/$defs/order\"},\"type\":\"array\"},\"plain\":true},\"required\":[\"orders\"],\"type\":\"object\"}");
    expect(JSON.stringify(tool.outputSchema)).toBe("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{\"items\":{\"items\":{\"properties\":{\"data\":{\"$defs\":{\"i\":{\"type\":\"object\"}},\"items\":{\"$ref\":\"#/properties/items/items/properties/data/$defs/i\"},\"type\":\"array\"},\"files\":{\"type\":\"array\"},\"id\":{\"type\":\"string\"}},\"required\":[\"id\",\"data\",\"files\"],\"type\":\"object\"},\"type\":\"array\"},\"meta\":{\"type\":\"object\"}},\"required\":[\"meta\",\"items\"],\"type\":\"object\"}");
  });

  it("draws a reference into another file of the tree as the workflow writes it, the console holding no file to embed", () => {
    const tool = toolOf({ inputs: { customers: { schema: { $ref: "./schemas/customers.json#/$defs/customer" } } }, mcp: { description: "Look a customer up." } } as unknown as Graph, "customers")!;
    expect(tool.inputSchema).toEqual({ $schema: "https://json-schema.org/draft/2020-12/schema", additionalProperties: false, properties: { customers: { $ref: "./schemas/customers.json#/$defs/customer" } }, type: "object" });
  });
});
