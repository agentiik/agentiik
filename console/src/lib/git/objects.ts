import { inflate } from "./inflate";

// Git's objects and packs, as much of them as committing one file from the browser takes: reading
// the pack a fetch answers, its deltas resolved, and writing the few objects a commit adds.

export type ObjectType = "commit" | "tree" | "blob" | "tag";
export type GitObject = { type: ObjectType; data: Uint8Array };
export type TreeEntry = { mode: string; name: string; id: string };

const types: Record<number, ObjectType> = { 1: "commit", 2: "tree", 3: "blob", 4: "tag" };
const codes: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

export function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function unhex(id: string): Uint8Array {
  const out = new Uint8Array(id.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(id.slice(i * 2, i * 2 + 2), 16);
  return out;
}

async function sha1(data: Uint8Array): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.digest("SHA-1", data as BufferSource));
}

// idOf is an object's name: the SHA-1 of its type, its length and its bytes.
export async function idOf(o: GitObject): Promise<string> {
  return hex(await sha1(concat([encoder.encode(`${o.type} ${o.data.length}\0`), o.data])));
}

// readPack is every object a pack holds, by name, each delta applied to its base.
export async function readPack(pack: Uint8Array): Promise<Map<string, GitObject>> {
  if (decoder.decode(pack.subarray(0, 4)) !== "PACK") throw new Error("the answer is not a pack");
  const view = new DataView(pack.buffer, pack.byteOffset, pack.byteLength);
  const version = view.getUint32(4);
  if (version !== 2 && version !== 3) throw new Error(`the pack is of version ${version}, which git does not write`);
  const count = view.getUint32(8);
  const atOffset = new Map<number, GitObject>();
  const waiting: { base: string; delta: Uint8Array }[] = [];
  let pos = 12;
  for (let n = 0; n < count; n++) {
    const start = pos;
    let c = pack[pos++]!;
    const kind = (c >> 4) & 7;
    let size = c & 15;
    let shift = 4;
    while (c & 0x80) {
      c = pack[pos++]!;
      size += (c & 0x7f) * 2 ** shift;
      shift += 7;
    }
    if (kind === 6) {
      c = pack[pos++]!;
      let back = c & 0x7f;
      while (c & 0x80) {
        c = pack[pos++]!;
        back = (back + 1) * 128 + (c & 0x7f);
      }
      const { data, end } = inflate(pack, pos, size);
      pos = end;
      const base = atOffset.get(start - back);
      if (!base) throw new Error("a delta of the pack names a base it does not hold");
      atOffset.set(start, { type: base.type, data: applyDelta(base.data, data) });
    } else if (kind === 7) {
      const base = hex(pack.subarray(pos, pos + 20));
      pos += 20;
      const { data, end } = inflate(pack, pos, size);
      pos = end;
      waiting.push({ base, delta: data });
    } else {
      const type = types[kind];
      if (!type) throw new Error(`an object of the pack is of kind ${kind}, which git does not write`);
      const { data, end } = inflate(pack, pos, size);
      pos = end;
      atOffset.set(start, { type, data });
    }
  }
  const byId = new Map<string, GitObject>();
  for (const o of atOffset.values()) byId.set(await idOf(o), o);
  // A delta against a base named by its id may come before its base, or after another such delta.
  while (waiting.length) {
    const before = waiting.length;
    for (let i = waiting.length - 1; i >= 0; i--) {
      const w = waiting[i]!;
      const base = byId.get(w.base);
      if (!base) continue;
      const o = { type: base.type, data: applyDelta(base.data, w.delta) };
      byId.set(await idOf(o), o);
      waiting.splice(i, 1);
    }
    if (waiting.length === before) throw new Error("a delta of the pack names a base it does not hold");
  }
  return byId;
}

// applyDelta is git's delta: the base's size and the result's, then copies out of the base and
// bytes inserted.
export function applyDelta(base: Uint8Array, delta: Uint8Array): Uint8Array {
  let pos = 0;
  const size = () => {
    let n = 0;
    let shift = 0;
    let c: number;
    do {
      c = delta[pos++]!;
      n += (c & 0x7f) * 2 ** shift;
      shift += 7;
    } while (c & 0x80);
    return n;
  };
  if (size() !== base.length) throw new Error("a delta of the pack is for another base");
  const out = new Uint8Array(size());
  let at = 0;
  while (pos < delta.length) {
    const op = delta[pos++]!;
    if (op & 0x80) {
      let offset = 0;
      let len = 0;
      for (let i = 0; i < 4; i++) if (op & (1 << i)) offset |= delta[pos++]! << (8 * i);
      for (let i = 0; i < 3; i++) if (op & (0x10 << i)) len |= delta[pos++]! << (8 * i);
      if (len === 0) len = 0x10000;
      out.set(base.subarray(offset >>> 0, (offset >>> 0) + len), at);
      at += len;
    } else if (op) {
      out.set(delta.subarray(pos, pos + op), at);
      at += op;
      pos += op;
    } else {
      throw new Error("a delta of the pack holds an operation git does not write");
    }
  }
  if (at !== out.length) throw new Error("a delta of the pack comes to another size than it says");
  return out;
}

async function deflate(data: Uint8Array): Promise<Uint8Array> {
  const stream = new Blob([data as BlobPart]).stream().pipeThrough(new CompressionStream("deflate"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

// writePack is a pack of whole objects, each compressed on its own, and the SHA-1 of it all.
export async function writePack(objects: GitObject[]): Promise<Uint8Array> {
  const header = new Uint8Array(12);
  header.set(encoder.encode("PACK"));
  const view = new DataView(header.buffer);
  view.setUint32(4, 2);
  view.setUint32(8, objects.length);
  const parts: Uint8Array[] = [header];
  for (const o of objects) {
    const head: number[] = [];
    let size = o.data.length;
    let c = (codes[o.type] << 4) | (size & 15);
    size = Math.floor(size / 16);
    while (size > 0) {
      head.push(c | 0x80);
      c = size & 0x7f;
      size = Math.floor(size / 128);
    }
    head.push(c);
    parts.push(new Uint8Array(head), await deflate(o.data));
  }
  const body = concat(parts);
  return concat([body, await sha1(body)]);
}

export type Commit = { tree: string; parents: string[]; message: string };

export function parseCommit(data: Uint8Array): Commit {
  const text = decoder.decode(data);
  const cut = text.indexOf("\n\n");
  const head = (cut < 0 ? text : text.slice(0, cut)).split("\n");
  const tree = head.find((l) => l.startsWith("tree "))?.slice(5);
  if (!tree) throw new Error("a commit of the repository names no tree");
  return { tree, parents: head.filter((l) => l.startsWith("parent ")).map((l) => l.slice(7)), message: cut < 0 ? "" : text.slice(cut + 2) };
}

export function parseTree(data: Uint8Array): TreeEntry[] {
  const out: TreeEntry[] = [];
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    const nul = data.indexOf(0, space);
    if (space < 0 || nul < 0 || nul + 21 > data.length) throw new Error("a tree of the repository is cut short");
    out.push({ mode: decoder.decode(data.subarray(pos, space)), name: decoder.decode(data.subarray(space + 1, nul)), id: hex(data.subarray(nul + 1, nul + 21)) });
    pos = nul + 21;
  }
  return out;
}

// writeTree is a tree's bytes, its entries in git's order: by name, a tree's as though it ended in
// a slash.
export function writeTree(entries: TreeEntry[]): Uint8Array {
  const key = (e: TreeEntry) => encoder.encode(e.mode === "40000" ? `${e.name}/` : e.name);
  const sorted = [...entries].sort((a, b) => {
    const x = key(a);
    const y = key(b);
    for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i]! - y[i]!;
    return x.length - y.length;
  });
  return concat(sorted.flatMap((e) => [encoder.encode(`${e.mode} ${e.name}\0`), unhex(e.id)]));
}

export type Person = { name: string; email: string; when: Date };

// signature is a person as a commit writes them: name, address, seconds since the epoch and the
// offset of the time zone they were in.
function signature(p: Person): string {
  const offset = -p.when.getTimezoneOffset();
  const sign = offset < 0 ? "-" : "+";
  const abs = Math.abs(offset);
  const zone = `${sign}${String(Math.floor(abs / 60)).padStart(2, "0")}${String(abs % 60).padStart(2, "0")}`;
  const clean = (s: string) => s.replace(/[<>\n]/g, "").trim();
  return `${clean(p.name)} <${clean(p.email)}> ${Math.floor(p.when.getTime() / 1000)} ${zone}`;
}

export function writeCommit(c: { tree: string; parents: string[]; author: Person; message: string }): Uint8Array {
  const lines = [`tree ${c.tree}`, ...c.parents.map((p) => `parent ${p}`), `author ${signature(c.author)}`, `committer ${signature(c.author)}`];
  const message = c.message.endsWith("\n") ? c.message : `${c.message}\n`;
  return encoder.encode(`${lines.join("\n")}\n\n${message}`);
}
