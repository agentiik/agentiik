// What the files' ref field offers: the default branch, then the repository's other branches and
// its tags as GET /api/v1/{ns}/workflows/{name}/refs lists them, each by its short name and who
// moved it last, then the versions of the history. A name that is both a branch and a tag is
// offered in full, since the API refuses it short with a 400 rather than guess which is meant.

import { clock } from "./format";

export type Ref = { name: string; commit: string | null; protected: boolean; moved_by?: string; moved_at?: string };
export type Offer = { value: string; label: string };

const heads = "refs/heads/";
const tags = "refs/tags/";

export function offers(refs: Ref[], defaultBranch: string, versions: { commit: string; subject?: string }[], now: number): Offer[] {
  const short = (name: string) => (name.startsWith(heads) ? name.slice(heads.length) : name.startsWith(tags) ? name.slice(tags.length) : name);
  const held = refs.filter((r) => r.commit !== null);
  const count = new Map<string, number>();
  for (const r of held) count.set(short(r.name), (count.get(short(r.name)) ?? 0) + 1);

  const first = (count.get(defaultBranch) ?? 0) > 1 ? heads + defaultBranch : defaultBranch;
  const out: Offer[] = [{ value: first, label: "the default branch" }];
  const seen = new Set([first]);
  const kinds: [string, string][] = [
    [heads, "branch"],
    [tags, "tag"],
  ];
  for (const [prefix, kind] of kinds) {
    for (const r of held) {
      if (!r.name.startsWith(prefix) || r.name === heads + defaultBranch) continue;
      const name = short(r.name);
      const value = count.get(name)! > 1 ? r.name : name;
      if (seen.has(value)) continue;
      seen.add(value);
      const moved = r.moved_by && r.moved_at ? `, moved by ${r.moved_by} at ${clock(r.moved_at, now)}` : "";
      out.push({ value, label: `${kind}${moved}` });
    }
  }
  for (const v of versions) {
    if (seen.has(v.commit)) continue;
    seen.add(v.commit);
    out.push({ value: v.commit, label: v.subject ?? v.commit.slice(0, 7) });
  }
  return out;
}
