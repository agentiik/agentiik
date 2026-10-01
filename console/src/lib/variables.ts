// A namespace variable's value is any JSON value, and the console shows it and takes it as JSON, so
// that 30 is a number and "30" a string, as they are in a workflow file.

// shown is a value as one line of JSON, as the API holds it.
export function shown(value: unknown): string {
  return JSON.stringify(value) ?? "null";
}

// parsed is what a value typed as JSON holds, or why it holds nothing: the sentence names what to
// write instead, since text alone, the commonest slip, is JSON once it is quoted.
export function parsed(text: string): { ok: true; value: unknown } | { ok: false; why: string } {
  const t = text.trim();
  if (t === "") return { ok: false, why: "The value is empty. Write null for no value." };
  try {
    return { ok: true, value: JSON.parse(t) };
  } catch {
    return { ok: false, why: `The value is not JSON. Text is written in double quotes: ${JSON.stringify(t)}.` };
  }
}
