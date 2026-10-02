// The git binary and the files it works on, for tests/git.test.ts: the console's git is held to
// git's own, and these are what reaching git from a test takes, written in JavaScript as
// tests/serve.js is, since the console's types are a browser's and know nothing of Node.

import { execFileSync, spawnSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { deflateRawSync, deflateSync } from "node:zlib";

const made = [];

export function cleanUp() {
  for (const d of made.splice(0)) rmSync(d, { recursive: true, force: true });
}

export function dir() {
  const d = mkdtempSync(join(tmpdir(), "agk-git-"));
  made.push(d);
  return d;
}

// The environment git runs in: no configuration but its own, and a fixed author.
const env = { ...process.env, GIT_AUTHOR_NAME: "Alice Martin", GIT_AUTHOR_EMAIL: "alice@example.com", GIT_COMMITTER_NAME: "Alice Martin", GIT_COMMITTER_EMAIL: "alice@example.com", GIT_CONFIG_NOSYSTEM: "1", HOME: tmpdir() };

export function git(cwd, ...args) {
  return execFileSync("git", args, { cwd, env, encoding: "utf8" }).trim();
}

export function gitBytes(cwd, ...args) {
  return new Uint8Array(execFileSync("git", args, { cwd, env }));
}

export function gitIn(cwd, input, ...args) {
  const r = spawnSync("git", args, { cwd, env, input });
  if (r.status !== 0) throw new Error(r.stderr.toString());
  return new Uint8Array(r.stdout);
}

export function write(path, content, executable = false) {
  mkdirSync(join(path, ".."), { recursive: true });
  writeFileSync(path, content);
  if (executable) chmodSync(path, 0o755);
}

export function joined(...parts) {
  return join(...parts);
}

export function zlib(data, level) {
  return new Uint8Array(deflateSync(data, { level }));
}

export function rawDeflate(data) {
  return new Uint8Array(deflateRawSync(data));
}
