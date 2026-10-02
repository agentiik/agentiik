// A workflow's tree as GET /api/v1/{ns}/workflows/{name}/tree/{ref} lists it, drawn as folders, two
// trees compared file by file, and two versions of a file compared line by line.

export type Entry = { path: string; mode: string; size: number; sha256: string };

export type Folder = { kind: "folder"; name: string; path: string; children: Node[] };
export type File = { kind: "file"; name: string; entry: Entry };
export type Node = Folder | File;

// folders is the listing as nested folders, each folder's folders first and then its files, both by
// name, as a file browser draws them; the API lists files alone, a folder being the path they share.
export function folders(entries: Entry[]): Node[] {
  const root: Folder = { kind: "folder", name: "", path: "", children: [] };
  for (const entry of entries) {
    const parts = entry.path.split("/");
    let at = root;
    for (const [i, part] of parts.slice(0, -1).entries()) {
      const path = parts.slice(0, i + 1).join("/");
      let next = at.children.find((n): n is Folder => n.kind === "folder" && n.name === part);
      if (!next) {
        next = { kind: "folder", name: part, path, children: [] };
        at.children.push(next);
      }
      at = next;
    }
    at.children.push({ kind: "file", name: parts[parts.length - 1]!, entry });
  }
  const order = (nodes: Node[]) => {
    nodes.sort((a, b) => (a.kind === b.kind ? (a.name < b.name ? -1 : a.name > b.name ? 1 : 0) : a.kind === "folder" ? -1 : 1));
    for (const n of nodes) if (n.kind === "folder") order(n.children);
  };
  order(root.children);
  return root.children;
}

// foldersOf is every folder a path lies in, from the root, which a browser opens to show the path.
export function foldersOf(path: string): string[] {
  const parts = path.split("/").slice(0, -1);
  return parts.map((_, i) => parts.slice(0, i + 1).join("/"));
}

export type Change =
  | { kind: "added"; path: string; after: Entry }
  | { kind: "removed"; path: string; before: Entry }
  | { kind: "modified"; path: string; before: Entry; after: Entry };

// changes is what differs between two trees, by path: a file in one and not the other, and a file in
// both whose bytes or mode differ. The SHA-256 the listing gives each file says whether its bytes
// differ without reading them, so a tree of many files is compared in one read of each listing.
export function changes(before: Entry[], after: Entry[]): Change[] {
  const was = new Map(before.map((e) => [e.path, e]));
  const is = new Map(after.map((e) => [e.path, e]));
  const out: Change[] = [];
  for (const [path, a] of is) {
    const b = was.get(path);
    if (!b) out.push({ kind: "added", path, after: a });
    else if (b.sha256 !== a.sha256 || b.mode !== a.mode) out.push({ kind: "modified", path, before: b, after: a });
  }
  for (const [path, b] of was) {
    if (!is.has(path)) out.push({ kind: "removed", path, before: b });
  }
  return out.sort((x, y) => (x.path < y.path ? -1 : x.path > y.path ? 1 : 0));
}

export type Line = { kind: "same" | "removed" | "added"; text: string; before?: number; after?: number };
export type Hunk = { lines: Line[] };

// linesOf is a file's text as lines, a last newline ending the last line rather than opening another.
export function linesOf(text: string): string[] {
  if (text === "") return [];
  return text.replace(/\n$/, "").split("\n");
}

// lineDiff is the shortest edit from one text to the other, by Myers's algorithm: the lines removed,
// added and kept, in order, each numbered in the file it is in. Shortest, so that a line moved is read
// as the one change a person made rather than as everything around it rewritten.
export function lineDiff(before: string, after: string): Line[] {
  const a = linesOf(before);
  const b = linesOf(after);
  const n = a.length;
  const m = b.length;
  const max = n + m;
  const offset = max + 1;
  const v = new Array<number>(2 * max + 3).fill(0);
  const trace: number[][] = [];
  let found = false;
  for (let d = 0; d <= max && !found; d++) {
    trace.push(v.slice());
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[offset + k - 1]! < v[offset + k + 1]!) ? v[offset + k + 1]! : v[offset + k - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        found = true;
        break;
      }
    }
  }
  if (!found) trace.push(v.slice());
  // Walk the trace back from the end, each step a line kept, removed or added.
  const out: Line[] = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d > 0; d--) {
    const w = trace[d]!;
    const k = x - y;
    const prevK = k === -d || (k !== d && w[offset + k - 1]! < w[offset + k + 1]!) ? k + 1 : k - 1;
    const prevX = w[offset + prevK]!;
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      out.push({ kind: "same", text: a[x - 1]!, before: x, after: y });
      x--;
      y--;
    }
    if (x === prevX) out.push({ kind: "added", text: b[y - 1]!, after: y });
    else out.push({ kind: "removed", text: a[x - 1]!, before: x });
    x = prevX;
    y = prevY;
  }
  while (x > 0 && y > 0) {
    out.push({ kind: "same", text: a[x - 1]!, before: x, after: y });
    x--;
    y--;
  }
  return out.reverse();
}

// hunks is a diff cut to what changed and the lines around it, context lines each side, as a review
// reads it: the rest of the file is the same in both and would only push the change off the screen.
// Two changes whose contexts meet are one hunk.
export function hunks(lines: Line[], context = 3): Hunk[] {
  const kept = new Array<boolean>(lines.length).fill(false);
  lines.forEach((l, i) => {
    if (l.kind === "same") return;
    for (let j = Math.max(0, i - context); j <= Math.min(lines.length - 1, i + context); j++) kept[j] = true;
  });
  const out: Hunk[] = [];
  let current: Line[] = [];
  lines.forEach((l, i) => {
    if (kept[i]) {
      current.push(l);
    } else if (current.length > 0) {
      out.push({ lines: current });
      current = [];
    }
  });
  if (current.length > 0) out.push({ lines: current });
  return out;
}

// binary says whether bytes are not text a person reads: a NUL among the first 8000, as git decides
// it, or bytes that are not UTF-8.
export function binary(bytes: Uint8Array): boolean {
  if (bytes.subarray(0, 8000).includes(0)) return true;
  try {
    new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return false;
  } catch {
    return true;
  }
}

// shownUpTo is the largest file the browser draws, 1 MiB: past it a page would lay out tens of
// thousands of lines nobody reads in a browser, and the file is offered to download instead.
export const shownUpTo = 1 << 20;

// sizeOf is a file's size as a person reads it.
export function sizeOf(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1 << 20) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1 << 20)).toFixed(1)} MiB`;
}
