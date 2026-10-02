import { Told } from "../problem";
import { idOf, parseCommit, parseTree, readPack, writeCommit, writePack, writeTree, type GitObject, type Person, type TreeEntry } from "./objects";
import { fetchPack, push, refs, zero, type Remote } from "./protocol";

// Committing one file from the browser, as a clone would: the repository fetched over git, the file
// written into the tree of the commit it was edited from, and a commit pushed onto a branch, the
// pre-receive hook checking it as it checks any push. Nothing reaches the repository but over git,
// so the console holds no route of its own for writing and no rule a push would not hold too.

export type Commit = {
  remote: Remote;
  // the commit the file was opened at, which the new one follows.
  parent: string;
  // the branch committed to, and whether it is new, starting at parent.
  branch: string;
  create: boolean;
  // the file's path in the tree, and what it now holds.
  path: string;
  text: string;
  message: string;
  author: Person;
};

// commitFile commits and pushes, answering the new commit's name, or throws what the repository or
// its hook said, as it said it.
export async function commitFile(c: Commit): Promise<string> {
  const ref = `refs/heads/${c.branch}`;
  const tips = await refs(c.remote);
  const tip = tips.get(ref);
  if (c.create && tip) throw new Told(`${c.branch} is already a branch of the repository.`);
  if (!c.create && tip !== c.parent) throw new Told(tip ? `${c.branch} has moved since the file was opened: open it again to edit what it holds now.` : `${c.branch} is not a branch of the repository.`);

  const objects = await readPack(await fetchPack(c.remote, c.parent));
  const parent = objects.get(c.parent);
  if (!parent || parent.type !== "commit") throw new Error(`the repository did not send ${c.parent}`);

  const added: GitObject[] = [];
  const blob: GitObject = { type: "blob", data: new TextEncoder().encode(c.text) };
  added.push(blob);
  const tree = await replace(objects, parseCommit(parent.data).tree, c.path.split("/"), await idOf(blob), added);
  const commit: GitObject = { type: "commit", data: writeCommit({ tree, parents: [c.parent], author: c.author, message: c.message }) };
  added.push(commit);
  const id = await idOf(commit);

  const pushed = await push(c.remote, { from: c.create ? zero : c.parent, to: id, ref }, await writePack(added));
  if (!pushed.ok) {
    // What the hook said is the reason a person reads; the protocol's word for it is added only
    // where it says something the hook did not.
    const said = pushed.said.map((s) => s.replace(/^error:\s*/, "")).join(" ");
    const reason = [said, said.includes(pushed.reason.replace(/^refused:\s*/, "")) ? "" : pushed.reason].filter(Boolean).join(" ");
    throw new Told(reason || "the repository refused the push and said nothing more");
  }
  return id;
}

// replace writes the blob at path into the tree named, a new one where none is, and every tree above
// it, answering the new root's name; the trees it writes are added to what the push sends.
async function replace(objects: Map<string, GitObject>, treeId: string | null, path: string[], blob: string, added: GitObject[]): Promise<string> {
  let entries: TreeEntry[] = [];
  if (treeId) {
    const tree = objects.get(treeId);
    if (!tree || tree.type !== "tree") throw new Error(`the repository did not send the tree ${treeId}`);
    entries = parseTree(tree.data);
  }
  const [name, ...rest] = path;
  if (!name) throw new Error("a file is committed at a path");
  const at = entries.findIndex((e) => e.name === name);
  const old = at >= 0 ? entries[at]! : undefined;
  const entry =
    rest.length === 0
      ? { mode: old?.mode === "100755" ? "100755" : "100644", name, id: blob }
      : { mode: "40000", name, id: await replace(objects, old?.mode === "40000" ? old.id : null, rest, blob, added) };
  if (at >= 0) entries[at] = entry;
  else entries.push(entry);
  const written: GitObject = { type: "tree", data: writeTree(entries) };
  added.push(written);
  return idOf(written);
}
