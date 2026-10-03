import type { Graph } from "./graph";

// The tool a workflow is, as a client of a collection holding it is told of it: "one block, one
// tool, whose arguments are the workflow's inputs", each by its name and with its schema as it
// stands, required where the input is required and has no default; and, where the block names an
// output and the tool waits for its run, its schema as the outputSchema, which an async tool, whose
// call answers the run and never the output, does not publish. The engine builds the same from the
// same version, and this draws it for a person before any client is given it.

export type Tool = {
  name: string;
  title?: string;
  description: string;
  output?: string;
  mode: "sync" | "async";
  timeout?: string;
  annotations: Record<string, boolean>;
  inputSchema: Record<string, unknown>;
  outputSchema?: unknown;
};

type Block = {
  name?: string;
  title?: string;
  description: string;
  output?: string;
  mode?: "sync" | "async";
  timeout?: string;
  annotations?: Record<string, boolean>;
};

// toolOf is the tool a version's graph publishes, or null where its entry point declares no mcp
// block, or one written to the list of tools the language had before v0.7.0, which publishes none.
export function toolOf(graph: Graph, workflow: string): Tool | null {
  const block = graph.mcp as unknown as Block | undefined;
  if (!block || typeof block.description !== "string" || "tools" in block) return null;
  const inputs = (graph.inputs ?? {}) as Record<string, { schema?: unknown; required?: boolean; default?: unknown }>;
  const outputs = (graph.outputs ?? {}) as Record<string, { schema?: unknown }>;
  const properties: Record<string, unknown> = {};
  const required: string[] = [];
  for (const [name, input] of Object.entries(inputs)) {
    properties[name] = input.schema ?? true;
    if (input.required && input.default === undefined) required.push(name);
  }
  const inputSchema: Record<string, unknown> = { type: "object", properties };
  if (required.length) inputSchema.required = required;
  const mode = block.mode ?? "sync";
  return {
    name: block.name ?? workflow,
    title: block.title,
    description: block.description,
    output: block.output,
    mode,
    timeout: block.timeout,
    annotations: block.annotations ?? {},
    inputSchema,
    outputSchema: block.output && mode === "sync" ? outputs[block.output]?.schema : undefined,
  };
}
