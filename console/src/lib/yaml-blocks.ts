// Where each step's block is in a workflow file, read off its lines: what lets the graph select a
// block and a block select its node. The file is read as text and never parsed whole, since all
// this needs is the keys under steps and where each one ends, and a step a file includes from
// elsewhere simply has no block here.

export type Block = { start: number; end: number };

const indentOf = (line: string) => line.length - line.trimStart().length;
const quiet = (line: string) => line.trim() === "" || line.trimStart().startsWith("#");

// blocks are the steps of a workflow file by name, each from the line of its key to its last line,
// both counted from 1, a hidden block's name, which begins with a dot, among them.
export function blocks(text: string): Map<string, Block> {
  const lines = text.split("\n");
  const out = new Map<string, Block>();
  const steps = lines.findIndex((l) => /^steps:\s*(#.*)?$/.test(l));
  if (steps < 0) return out;
  let inner = -1;
  let open: { name: string; start: number } | undefined;
  let last = steps;
  const close = (end: number) => {
    if (open) out.set(open.name, { start: open.start + 1, end: end + 1 });
    open = undefined;
  };
  for (let i = steps + 1; i < lines.length; i++) {
    const line = lines[i]!;
    if (quiet(line)) continue;
    const indent = indentOf(line);
    if (inner < 0) inner = indent;
    if (indent === 0 || indent < inner) break;
    if (indent === inner) {
      const key = /^\s*("([^"]+)"|'([^']+)'|([^\s:#][^:#]*?))\s*:/.exec(line);
      if (key) {
        close(last);
        open = { name: key[2] ?? key[3] ?? key[4]!, start: i };
      }
    }
    last = i;
  }
  close(last);
  return out;
}

// stepAt is the step whose block holds a line, counted from 1.
export function stepAt(found: Map<string, Block>, line: number): string | undefined {
  for (const [name, b] of found) if (line >= b.start && line <= b.end) return name;
  return undefined;
}
