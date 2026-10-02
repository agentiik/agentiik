import { describe, expect, it } from "vitest";
import { binary, changes, folders, foldersOf, hunks, lineDiff, linesOf, sizeOf, type Entry } from "../src/lib/tree";

const e = (path: string, sha256 = "a", mode = "0644", size = 1): Entry => ({ path, mode, size, sha256 });

describe("folders", () => {
  it("nests the files under the folders their paths share, folders first, each by name", () => {
    const nodes = folders([e("agentiik.yaml"), e("scripts/normalize.py"), e("README.md"), e("schemas/customer.json"), e("schemas/customers.json"), e("scripts/lib/vat.py")]);
    expect(nodes.map((n) => `${n.kind} ${n.name}`)).toEqual(["folder schemas", "folder scripts", "file README.md", "file agentiik.yaml"]);
    const scripts = nodes[1]!;
    expect(scripts.kind === "folder" && scripts.children.map((n) => `${n.kind} ${n.name}`)).toEqual(["folder lib", "file normalize.py"]);
    expect(scripts.kind === "folder" && scripts.children[0]!.kind === "folder" && scripts.children[0]!.path).toBe("scripts/lib");
  });

  it("names the folders a path lies in, from the root", () => {
    expect(foldersOf("scripts/lib/vat.py")).toEqual(["scripts", "scripts/lib"]);
    expect(foldersOf("agentiik.yaml")).toEqual([]);
  });
});

describe("changes", () => {
  it("lists what was added, removed and modified, by path, a mode alone counting", () => {
    const before = [e("agentiik.yaml", "1"), e("old.py"), e("run.sh", "s", "0644"), e("same.txt", "x")];
    const after = [e("agentiik.yaml", "2"), e("new.py"), e("run.sh", "s", "0755"), e("same.txt", "x")];
    expect(changes(before, after).map((c) => `${c.kind} ${c.path}`)).toEqual(["modified agentiik.yaml", "added new.py", "removed old.py", "modified run.sh"]);
  });
});

describe("lineDiff", () => {
  it("is the shortest edit, each line numbered in its file", () => {
    const d = lineDiff("a\nb\nc\nd\n", "a\nc\nd\ne\n");
    expect(d.map((l) => `${l.kind[0]} ${l.text} ${l.before ?? "-"} ${l.after ?? "-"}`)).toEqual(["s a 1 1", "r b 2 -", "s c 3 2", "s d 4 3", "a e - 4"]);
  });

  it("reads a whole file added or removed, and two equal files as the same", () => {
    expect(lineDiff("", "x\ny\n").map((l) => l.kind)).toEqual(["added", "added"]);
    expect(lineDiff("x\n", "").map((l) => l.kind)).toEqual(["removed"]);
    expect(lineDiff("x\ny", "x\ny\n").every((l) => l.kind === "same")).toBe(true);
    expect(linesOf("")).toEqual([]);
  });

  it("reads a line changed as one removed and one added, the rest kept", () => {
    const before = "rounding: per_invoice\ncurrency: EUR\n";
    const after = "rounding: per_line\ncurrency: EUR\n";
    expect(lineDiff(before, after).map((l) => l.kind)).toEqual(["removed", "added", "same"]);
  });
});

describe("hunks", () => {
  it("keeps three lines of context each side, and joins changes whose contexts meet", () => {
    const before = Array.from({ length: 30 }, (_, i) => `line ${i + 1}`).join("\n");
    const lines = before.split("\n");
    lines[4] = "changed 5";
    lines[9] = "changed 10";
    lines[25] = "changed 26";
    const h = hunks(lineDiff(before, lines.join("\n")));
    expect(h).toHaveLength(2);
    expect(h[0]!.lines[0]!.before).toBe(2);
    expect(h[0]!.lines.at(-1)!.before).toBe(13);
    expect(h[1]!.lines.filter((l) => l.kind !== "same").map((l) => l.text)).toEqual(["line 26", "changed 26"]);
  });
});

describe("binary", () => {
  it("is a NUL in the first 8000 bytes, or bytes that are not UTF-8", () => {
    expect(binary(new TextEncoder().encode("steps:\n  é: ok\n"))).toBe(false);
    expect(binary(new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x00]))).toBe(true);
    expect(binary(new Uint8Array([0xff, 0xfe, 0x41]))).toBe(true);
  });

  it("writes a size as a person reads it", () => {
    expect([sizeOf(512), sizeOf(2210), sizeOf(3 << 20)]).toEqual(["512 B", "2.2 KiB", "3.0 MiB"]);
  });
});
