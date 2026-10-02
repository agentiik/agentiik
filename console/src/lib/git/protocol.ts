import { Told } from "../problem";
import { concat } from "./objects";

// Git's smart protocol over HTTP, as the console speaks it to the installation it is served from:
// the refs a repository advertises, a fetch of everything a commit reaches, and a push of one ref.
// The requests carry the console's session as any of its requests does, and the Content-Type git
// sends, which a page of another site cannot send without asking first.

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export const zero = "0".repeat(40);

// pkt is one pkt-line: four hexadecimal digits of length, itself included, then the line.
export function pkt(line: string): Uint8Array {
  const body = encoder.encode(line);
  return concat([encoder.encode((body.length + 4).toString(16).padStart(4, "0")), body]);
}

export const flush = encoder.encode("0000");

type Line = { kind: "data"; data: Uint8Array } | { kind: "flush" };

export function readLines(bytes: Uint8Array): Line[] {
  const out: Line[] = [];
  let pos = 0;
  while (pos + 4 <= bytes.length) {
    const len = parseInt(decoder.decode(bytes.subarray(pos, pos + 4)), 16);
    if (Number.isNaN(len)) throw new Error("the answer is not in git's pkt-line framing");
    if (len < 4) {
      out.push({ kind: "flush" });
      pos += 4;
      continue;
    }
    out.push({ kind: "data", data: bytes.subarray(pos + 4, pos + len) });
    pos += len;
  }
  return out;
}

// bands splits what a side band carries: the pack or the report on the first, what the server says
// to the person on the second, and a fatal error on the third.
function bands(lines: Line[]): { data: Uint8Array; said: string[]; fatal: string } {
  const data: Uint8Array[] = [];
  const said: string[] = [];
  let fatal = "";
  for (const l of lines) {
    if (l.kind !== "data" || l.data.length === 0) continue;
    const band = l.data[0];
    const rest = l.data.subarray(1);
    if (band === 1) data.push(rest);
    else if (band === 2) said.push(decoder.decode(rest));
    else if (band === 3) fatal += decoder.decode(rest);
  }
  return { data: concat(data), said: said.join("").split(/\r|\n/).map((s) => s.trim()).filter(Boolean), fatal: fatal.trim() };
}

// The repository's address, as its clone URL gives it, resolved against the console's own origin
// so that the requests carry its session.
export type Remote = { url: string; fetch: typeof fetch };

async function answered(r: Response): Promise<Uint8Array> {
  if (!r.ok) {
    const said = (await r.text()).trim();
    throw new Told(said || `the repository answered ${r.status}`);
  }
  return new Uint8Array(await r.arrayBuffer());
}

// refs is what a repository's refs point at, as it advertises them to a push.
export async function refs(remote: Remote): Promise<Map<string, string>> {
  const r = await remote.fetch(`${remote.url}/info/refs?service=git-receive-pack`, { credentials: "same-origin" });
  const lines = readLines(await answered(r));
  const out = new Map<string, string>();
  for (const l of lines) {
    if (l.kind !== "data") continue;
    const text = decoder.decode(l.data).replace(/\n$/, "");
    if (text.startsWith("#")) continue;
    const line = text.split("\0")[0]!;
    const [id, name] = line.split(" ");
    if (id && name && id !== zero) out.set(name, id);
  }
  return out;
}

// fetchPack is the pack of every object a commit reaches, asked for with nothing in hand.
export async function fetchPack(remote: Remote, want: string): Promise<Uint8Array> {
  const body = concat([pkt(`want ${want} side-band-64k ofs-delta no-progress agent=agentiik-console\n`), flush, pkt("done\n")]);
  const r = await remote.fetch(`${remote.url}/git-upload-pack`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/x-git-upload-pack-request", Accept: "application/x-git-upload-pack-result" },
    body: body as BodyInit,
  });
  const lines = readLines(await answered(r));
  // The acknowledgement comes before the side band: NAK, since nothing was said to be in hand.
  const first = lines.findIndex((l) => l.kind === "data" && decoder.decode(l.data).startsWith("NAK"));
  const { data, fatal } = bands(lines.slice(first + 1));
  if (fatal) throw new Told(fatal);
  return data;
}

export type Pushed = { ok: boolean; reason: string; said: string[] };

// push sends one ref's update and the pack it needs, and answers what the repository reported: the
// ref updated, or the reason it was not, with whatever its hooks said on the way.
export async function push(remote: Remote, update: { from: string; to: string; ref: string }, pack: Uint8Array): Promise<Pushed> {
  const command = pkt(`${update.from} ${update.to} ${update.ref}\0report-status side-band-64k quiet agent=agentiik-console\n`);
  const r = await remote.fetch(`${remote.url}/git-receive-pack`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/x-git-receive-pack-request", Accept: "application/x-git-receive-pack-result" },
    body: concat([command, flush, pack]) as BodyInit,
  });
  const { data, said, fatal } = bands(readLines(await answered(r)));
  if (fatal) return { ok: false, reason: fatal, said };
  let reason = "";
  let ok = false;
  for (const l of readLines(data)) {
    if (l.kind !== "data") continue;
    const line = decoder.decode(l.data).replace(/\n$/, "");
    if (line.startsWith("unpack ") && line !== "unpack ok") reason = line.slice(7);
    else if (line === `ok ${update.ref}`) ok = true;
    else if (line.startsWith(`ng ${update.ref} `)) reason = line.slice(`ng ${update.ref} `.length);
  }
  return { ok: ok && !reason, reason, said };
}
