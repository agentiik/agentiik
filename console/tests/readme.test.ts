import { describe, expect, it } from "vitest";

// "Every dependency of the console is under a permissive licence, recorded with its reason in
// console/README.md, the place a reader of package.json, which holds no comment, looks for one." A
// dependency added without its row is one whose reason and licence nobody wrote down.

const files = import.meta.glob(["../README.md", "../package.json"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const readme = files["../README.md"]!;
const manifest = JSON.parse(files["../package.json"]!) as { dependencies: Record<string, string>; devDependencies: Record<string, string> };

// rows answers the rows of the table under a heading of the README's dependencies: the packages its
// first cell names, and the rest of the row.
function rows(heading: string): { names: string[]; why: string }[] {
  const from = readme.indexOf(heading);
  const table = readme.slice(from).split("\n\n")[1] ?? "";
  return table
    .split("\n")
    .slice(2)
    .map((row) => row.split(" | "))
    .map(([first = "", ...rest]) => ({ names: [...first.matchAll(/`([^`]+)`/g)].map((m) => m[1]!), why: rest.join(" | ") }));
}

describe("the console's dependencies", () => {
  it("are each recorded in the README with a reason, shipped or used to build alone as package.json says", () => {
    const shipped = rows("Shipped in the build:");
    const built = rows("Used to build and test it, never shipped:");
    expect(shipped.flatMap((r) => r.names).sort()).toEqual(Object.keys(manifest.dependencies).sort());
    expect(built.flatMap((r) => r.names).sort()).toEqual(Object.keys(manifest.devDependencies).sort());
  });

  it("are each under a permissive licence, named in its row", () => {
    const unnamed = [...rows("Shipped in the build:"), ...rows("Used to build and test it, never shipped:")].filter((r) => !/\b(MIT|ISC|Apache-2\.0|BSD-[23]-Clause|SIL Open Font License 1\.1)\b/.test(r.why));
    expect(unnamed.map((r) => r.names.join(", "))).toEqual([]);
  });
});
