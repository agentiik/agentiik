// @vitest-environment node
import { afterAll, describe, expect, it } from "vitest";
import { cleanUp, dir, git, gitBytes, gitIn, joined, rawDeflate, write, zlib } from "./git-cli";
import { commitFile } from "../src/lib/git/commit";
import { inflate } from "../src/lib/git/inflate";
import { concat, idOf, readPack, writePack, type GitObject } from "../src/lib/git/objects";
import { pkt } from "../src/lib/git/protocol";
import { Told } from "../src/lib/problem";

// The console's git, held to git's own: packs git writes are read whole, the packs the console
// writes are indexed and checked by git, and a commit is pushed to git's receive-pack as the
// installation's smart HTTP serves it, the requests run through git's stateless-rpc programs.

afterAll(cleanUp);

// A repository with history, the entry point written four times over so that the pack git writes
// holds deltas, and a file in a folder.
function repository(): { work: string; bare: string } {
  const work = dir();
  git(work, "init", "-q", "-b", "main");
  let yaml = "apiVersion: agentiik/v1\nkind: Workflow\nsteps:\n";
  for (let i = 0; i < 4; i++) {
    yaml += `  step${i}:\n    image: ghcr.io/acme/agk-step:${i}.0.0\n    params:\n      note: ${"a long enough line to make a delta worth writing ".repeat(4)}\n`;
    write(joined(work, "agentiik.yaml"), yaml);
    write(joined(work, "scripts", "normalize.py"), `print(${i})\n`);
    git(work, "add", "-A");
    git(work, "commit", "-q", "-m", `version ${i}`);
  }
  const bare = dir();
  git(bare, "clone", "-q", "--bare", work, ".");
  git(bare, "repack", "-adq");
  return { work, bare };
}

// served answers the console's requests as an installation does, by git's stateless-rpc programs
// over the bare repository.
function served(bare: string, asked: string[] = []): typeof fetch {
  return async (input, init) => {
    const url = String(input);
    asked.push(`${init?.method ?? "GET"} ${url.slice(url.indexOf("/repo.git"))}`);
    const body = init?.body ? new Uint8Array(init.body as ArrayBufferLike) : new Uint8Array();
    let out: Uint8Array;
    if (url.endsWith("/info/refs?service=git-receive-pack")) {
      out = concat([pkt("# service=git-receive-pack\n"), new TextEncoder().encode("0000"), gitIn(bare, body, "receive-pack", "--stateless-rpc", "--advertise-refs", ".")]);
    } else if (url.endsWith("/git-upload-pack")) {
      out = gitIn(bare, body, "upload-pack", "--stateless-rpc", ".");
    } else if (url.endsWith("/git-receive-pack")) {
      out = gitIn(bare, body, "receive-pack", "--stateless-rpc", ".");
    } else {
      return new Response("no such thing", { status: 404 });
    }
    return new Response(out as BodyInit, { status: 200 });
  };
}

const alice = { name: "Alice Martin", email: "alice@example.com", when: new Date("2026-10-01T09:30:00Z") };

describe("inflate", () => {
  it("reads every kind of block deflate writes, and says where each stream ended", () => {
    const texts = [new Uint8Array(0), new TextEncoder().encode("steps:\n  a: {}\n"), new TextEncoder().encode("abcabcabc".repeat(5000)), Uint8Array.from({ length: 70000 }, (_, i) => (i * 7919) % 251)];
    for (const level of [0, 1, 6, 9]) {
      const streams = texts.map((t) => zlib(t, level));
      const all = concat([new Uint8Array([9, 9]), ...streams, new Uint8Array([7])]);
      let at = 2;
      texts.forEach((t, i) => {
        const { data, end } = inflate(all, at);
        expect(data).toEqual(t);
        expect(end).toBe(at + streams[i]!.length);
        at = end;
      });
      expect(all[at]).toBe(7);
    }
  });

  it("refuses what is not zlib", () => {
    expect(() => inflate(rawDeflate(new Uint8Array([1, 2, 3])), 0)).toThrow(/not zlib/);
  });
});

describe("packs", () => {
  it("reads a pack git wrote, deltas and all, naming each object as git does", async () => {
    const { bare } = repository();
    const pack = gitIn(bare, new TextEncoder().encode("main\n"), "pack-objects", "--stdout", "--revs", "-q");
    const objects = await readPack(pack);
    const listed = git(bare, "rev-list", "--objects", "main").split("\n").map((l: string) => l.split(" ")[0]);
    expect([...objects.keys()].sort()).toEqual([...listed].sort());
    const blob = git(bare, "rev-parse", "main:agentiik.yaml");
    expect(objects.get(blob)!.data).toEqual(gitBytes(bare, "cat-file", "blob", blob));
  });

  it("writes a pack git indexes, each object under the name git gives it", async () => {
    const bare = dir();
    git(bare, "init", "-q", "--bare");
    const objects: GitObject[] = [
      { type: "blob", data: new TextEncoder().encode("kind: Workflow\n") },
      { type: "blob", data: new Uint8Array(100000).fill(65) },
    ];
    gitIn(bare, await writePack(objects), "index-pack", "--stdin", "--strict");
    for (const o of objects) expect(git(bare, "cat-file", "-t", await idOf(o))).toBe("blob");
  });
});

describe("committing a file over git", () => {
  it("commits the file onto the branch it was opened from, as the person editing, and nothing else changes", async () => {
    const { bare } = repository();
    const parent = git(bare, "rev-parse", "main");
    const before = git(bare, "show", "main:agentiik.yaml");
    const asked: string[] = [];
    const id = await commitFile({
      remote: { url: "https://agentiik.example.com/finance/repo.git", fetch: served(bare, asked) },
      parent,
      branch: "main",
      create: false,
      path: "agentiik.yaml",
      text: `${before}\n  notify:\n    image: ghcr.io/acme/agk-notify:1.0.0\n`,
      message: "Add notify",
      author: alice,
    });
    expect(git(bare, "rev-parse", "main")).toBe(id);
    expect(git(bare, "log", "-1", "--format=%P%n%an <%ae>%n%s", "main")).toBe(`${parent}\nAlice Martin <alice@example.com>\nAdd notify`);
    expect(git(bare, "show", "main:agentiik.yaml")).toBe(`${before}\n  notify:\n    image: ghcr.io/acme/agk-notify:1.0.0`);
    expect(git(bare, "diff", "--name-only", parent, "main")).toBe("agentiik.yaml");
    git(bare, "fsck", "--strict", "--no-progress");
    expect(asked).toEqual(["GET /repo.git/info/refs?service=git-receive-pack", "POST /repo.git/git-upload-pack", "POST /repo.git/git-receive-pack"]);
  });

  it("commits a file in a folder onto a new branch, leaving the default branch where it was", async () => {
    const { bare } = repository();
    const parent = git(bare, "rev-parse", "main");
    const id = await commitFile({ remote: { url: "/finance/repo.git", fetch: served(bare) }, parent, branch: "try-retries", create: true, path: "scripts/normalize.py", text: "print('retried')\n", message: "Try retries", author: alice });
    expect(git(bare, "rev-parse", "try-retries")).toBe(id);
    expect(git(bare, "rev-parse", "main")).toBe(parent);
    expect(git(bare, "show", "try-retries:scripts/normalize.py")).toBe("print('retried')");
    git(bare, "fsck", "--strict", "--no-progress");
  });

  it("refuses a branch that moved since the file was opened, and a branch that already exists", async () => {
    const { bare } = repository();
    const older = git(bare, "rev-parse", "main~1");
    await expect(commitFile({ remote: { url: "/finance/repo.git", fetch: served(bare) }, parent: older, branch: "main", create: false, path: "agentiik.yaml", text: "kind: Workflow\n", message: "Late", author: alice })).rejects.toThrow("main has moved since the file was opened: open it again to edit what it holds now.");
    await expect(commitFile({ remote: { url: "/finance/repo.git", fetch: served(bare) }, parent: older, branch: "main", create: true, path: "agentiik.yaml", text: "kind: Workflow\n", message: "Again", author: alice })).rejects.toThrow("main is already a branch of the repository.");
  });

  it("says what the hook said when it refuses the push, and moves nothing", async () => {
    const { bare } = repository();
    write(joined(bare, "hooks", "pre-receive"), "#!/bin/sh\necho 'agentiik.yaml line 6: steps.notify needs normalize.done, a port normalize does not publish' >&2\nexit 1\n", true);
    const parent = git(bare, "rev-parse", "main");
    const refused = commitFile({ remote: { url: "/finance/repo.git", fetch: served(bare) }, parent, branch: "main", create: false, path: "agentiik.yaml", text: "kind: Workflow\n", message: "Broken", author: alice });
    await expect(refused).rejects.toBeInstanceOf(Told);
    await expect(refused).rejects.toThrow(/agentiik\.yaml line 6: steps\.notify needs normalize\.done, a port normalize does not publish/);
    expect(git(bare, "rev-parse", "main")).toBe(parent);
  });
});
