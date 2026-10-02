// A namespace variable is a name and a value, and the console writes the value as the text typed,
// whatever it holds, since a person setting a variable means the characters they wrote. The API holds
// any JSON value, which agk or a Terraform configuration may write, so one that is not text is shown
// as one line of JSON.

// shown is a value as the console shows it and offers it to edit: text as itself, anything else as JSON.
export function shown(value: unknown): string {
  return typeof value === "string" ? value : (JSON.stringify(value) ?? "null");
}

// written is the value a form sends: the text typed, except where it is what the variable already
// showed, which keeps a value that is not text as it is, so that changing who reads a list leaves it
// a list rather than the text of one.
export function written(typed: string, before?: { value: unknown }): unknown {
  if (before && typeof before.value !== "string" && typed === shown(before.value)) return before.value;
  return typed;
}
