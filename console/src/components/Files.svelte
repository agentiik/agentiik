<script lang="ts">
  import { untrack } from "svelte";
  import { refusal, type API } from "../api/client";
  import type { components } from "../api/schema";
  import { clock } from "../lib/format";
  import type { Place } from "../lib/place.svelte";
  import { binary, changes, folders, foldersOf, hunks, lineDiff, shownUpTo, sizeOf, type Change, type Entry, type Node } from "../lib/tree";
  import type { Problem } from "../lib/workflow-check";
  import Icon from "./Icon.svelte";

  // A workflow's repository as one ref holds it: its files as folders, and the file chosen in
  // JetBrains Mono on the sunken surface, numbered; or, compared with another ref, what differs
  // between the two trees, every file and not the entry point alone, and the file chosen line by
  // line. The ref, the file and the ref compared with are the address's query, so that a link to a
  // file at a branch opens that file at that branch.
  type Repository = components["schemas"]["workflowDetail"]["repository"];
  type HistoryEntry = components["schemas"]["historyEntry"];
  let {
    api,
    place,
    namespace,
    workflow,
    repository,
    history,
    mayRun,
    onrun,
  }: { api: API; place: Place; namespace: string; workflow: string; repository: Repository; history: HistoryEntry[]; mayRun: boolean; onrun: (ref: string) => void } = $props();

  const ref = $derived(place.query.get("ref") || repository.default_branch);
  const against = $derived(place.query.get("against") || "");
  const chosen = $derived(place.query.get("path") || "");

  type Listing = { commit: string; entries: Entry[] };
  let listing = $state<Listing | null>(null);
  let other = $state<Listing | null>(null);
  let refused = $state("");
  let typed = $state("");
  let comparing = $state("");
  let opened = $state<Set<string>>(new Set());

  // tree reads the listing a ref names, saying a ref that names nothing as the API answers it: a 404
  // is no branch, tag or version of that name, whatever the caller may read, since the workflow
  // itself was read to draw this page.
  async function tree(name: string): Promise<Listing | string> {
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows/{name}/tree/{ref}", { params: { path: { ns: namespace, name: workflow, ref: name } } });
    if (data && typeof data === "object" && "entries" in data) return { commit: data.commit, entries: data.entries };
    if (response.status === 404) return `No branch, tag or version of ${workflow} is named ${name}.`;
    return refusal(response, error).message;
  }

  // Each read is numbered, and an answer to one that has been asked again since is left: a ref
  // switched while the first is read would otherwise be drawn over by the first's late answer.
  let asked = 0;
  $effect(() => {
    const r = ref;
    const a = against;
    untrack(async () => {
      const n = ++asked;
      refused = "";
      listing = null;
      other = null;
      typed = r;
      comparing = a;
      const read = await tree(r);
      if (n !== asked) return;
      if (typeof read === "string") {
        refused = read;
        return;
      }
      listing = read;
      if (a) {
        const b = await tree(a);
        if (n !== asked) return;
        if (typeof b === "string") refused = b;
        else other = b;
      }
    });
  });

  // The file chosen, or the entry point where none is, which is what a workflow's tree is read for.
  const files = $derived(listing?.entries ?? []);
  const shown = $derived(chosen && files.some((f) => f.path === chosen) ? chosen : files.some((f) => f.path === "agentiik.yaml") ? "agentiik.yaml" : (files[0]?.path ?? ""));
  const entry = $derived(files.find((f) => f.path === shown));
  const nodes = $derived(folders(files));
  $effect(() => {
    const path = shown;
    untrack(() => {
      if (path) opened = new Set([...opened, ...foldersOf(path)]);
    });
  });

  // What a file holds, read when it is chosen: its text, or why it is not drawn.
  type Read = { text: string } | { not: string };
  async function bytesAt(name: string, path: string, size: number): Promise<Read> {
    if (size > shownUpTo) return { not: `${sizeOf(size)}, past the ${sizeOf(shownUpTo)} the console draws` };
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows/{name}/tree/{ref}", { params: { path: { ns: namespace, name: workflow, ref: name }, query: { path } }, parseAs: "arrayBuffer" });
    if (!(data instanceof ArrayBuffer)) return { not: `it could not be read: ${refusal(response, error).message}` };
    const bytes = new Uint8Array(data);
    if (binary(bytes)) return { not: `binary, ${sizeOf(bytes.length)}` };
    return { text: new TextDecoder().decode(bytes) };
  }

  let content = $state<Read | null>(null);
  let reading = 0;
  $effect(() => {
    const at = listing?.commit;
    const e = entry;
    if (against || !at || !e) return;
    untrack(async () => {
      const n = ++reading;
      content = null;
      const read = await bytesAt(at, e.path, e.size);
      if (n === reading) content = read;
    });
  });
  const lines = $derived(content && "text" in content ? content.text.replace(/\n$/, "").split("\n") : []);

  // The entry point held to workflow.schema.json as it is read, each problem on its line. The check
  // and the schema are loaded the first time a file is checked, so that a page that reads no
  // workflow file never downloads them.
  let problems = $state<Problem[] | null>(null);
  $effect(() => {
    const c = content;
    const path = entry?.path;
    problems = null;
    if (!c || !("text" in c) || path !== "agentiik.yaml") return;
    const text = c.text;
    untrack(async () => {
      const [{ YamlTree }, { check }] = await Promise.all([import("../lib/yaml-tree"), import("../lib/workflow-check")]);
      if (content === c) problems = check(new YamlTree(text));
    });
  });
  const wrong = $derived(new Set((problems ?? []).map((p) => p.line)));

  // Compared: what changed from the ref compared with to the ref shown, and the file chosen among
  // them read on both sides.
  const changed = $derived(listing && other ? changes(other.entries, listing.entries) : []);
  const change = $derived(changed.find((c) => c.path === chosen) ?? changed[0]);
  let sides = $state<{ before: Read | null; after: Read | null } | null>(null);
  $effect(() => {
    const c = change;
    const b = other?.commit;
    const a = listing?.commit;
    if (!c || !a || !b) {
      sides = null;
      return;
    }
    untrack(async () => {
      const n = ++reading;
      sides = null;
      const before = c.kind === "added" ? { text: "" } : await bytesAt(b, c.path, c.before.size);
      const after = c.kind === "removed" ? { text: "" } : await bytesAt(a, c.path, c.after.size);
      if (n === reading) sides = { before, after };
    });
  });
  const diffed = $derived(sides?.before && sides.after && "text" in sides.before && "text" in sides.after ? hunks(lineDiff(sides.before.text, sides.after.text)) : null);
  const counted = $derived(diffed ? diffed.flatMap((h) => h.lines).reduce((n, l) => ({ added: n.added + (l.kind === "added" ? 1 : 0), removed: n.removed + (l.kind === "removed" ? 1 : 0) }), { added: 0, removed: 0 }) : null);

  function narrow(set: Record<string, string | null>) {
    const q = new URLSearchParams(place.query);
    for (const [k, v] of Object.entries(set)) {
      if (v === null || v === "") q.delete(k);
      else q.set(k, v);
    }
    place.narrow(q);
  }

  function switchTo(e: SubmitEvent) {
    e.preventDefault();
    const name = typed.trim();
    narrow({ ref: name === repository.default_branch ? null : name, path: chosen || null });
  }

  function compare(e: SubmitEvent) {
    e.preventDefault();
    narrow({ against: comparing.trim(), path: null });
  }

  function toggle(path: string) {
    const next = new Set(opened);
    if (next.has(path)) next.delete(path);
    else next.add(path);
    opened = next;
  }

  const download = $derived(entry ? `api/v1/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflow)}/tree/${encodeURIComponent(listing?.commit ?? ref)}?path=${encodeURIComponent(entry.path)}` : "");
  const verb: Record<Change["kind"], string> = { added: "added", removed: "removed", modified: "modified" };
  // named is a ref as a sentence writes it: a commit by its first seven characters, as everywhere
  // else in the console, and a branch or a tag by its name.
  const named = (r: string) => (/^[0-9a-f]{40}$/.test(r) ? r.slice(0, 7) : r);
  const now = Date.now();
</script>

{#snippet branch(list: Node[], depth: number)}
  {#each list as node (node.kind === "folder" ? `d:${node.path}` : node.entry.path)}
    {#if node.kind === "folder"}
      <li>
        <button class="row folder" style="padding-left: {depth * 14 + 8}px" aria-expanded={opened.has(node.path)} onclick={() => toggle(node.path)}>
          <span class="caret" aria-hidden="true">{opened.has(node.path) ? "▾" : "▸"}</span>{node.name}
        </button>
        {#if opened.has(node.path)}<ul>{@render branch(node.children, depth + 1)}</ul>{/if}
      </li>
    {:else}
      <li>
        <button class="row leaf mono" style="padding-left: {depth * 14 + 22}px" aria-current={node.entry.path === shown ? "true" : undefined} onclick={() => narrow({ path: node.entry.path })}>
          <span>{node.name}</span><span class="faint size">{sizeOf(node.entry.size)}</span>
        </button>
      </li>
    {/if}
  {/each}
{/snippet}

<div class="bar">
  <form class="pick" onsubmit={switchTo}>
    <label for="files-ref" class="muted">At</label>
    <input id="files-ref" class="mono" list="files-refs" bind:value={typed} aria-label="Branch, tag or commit" />
    <button class="control" type="submit">Show</button>
  </form>
  <datalist id="files-refs">
    <option value={repository.default_branch}>the default branch</option>
    {#each history.filter((h) => h.version) as h (h.commit)}<option value={h.commit}>{h.subject ?? h.commit.slice(0, 7)}</option>{/each}
  </datalist>
  {#if listing}<span class="faint mono" title={listing.commit}>{listing.commit.slice(0, 7)}</span>{/if}
  <form class="pick" onsubmit={compare}>
    <label for="files-against" class="muted">Compared with</label>
    <input id="files-against" class="mono" list="files-refs" bind:value={comparing} placeholder="a branch, a tag or a commit" aria-label="Ref to compare with" />
    <button class="control" type="submit">Compare</button>
    {#if against}<button class="control" type="button" onclick={() => narrow({ against: null, path: null })}>Stop comparing</button>{/if}
  </form>
  {#if mayRun && listing && !against}
    <button class="control primary right" onclick={() => onrun(ref)}><Icon name="control-run" size={14} />Run {ref === repository.default_branch ? "the head" : ref}</button>
  {/if}
</div>

{#if refused}
  <p class="refused" role="alert">{refused}</p>
{:else if !listing}
  <p class="muted" role="status">Reading the tree at <span class="mono">{ref}</span>.</p>
{:else if against && other}
  <div class="columns">
    <section class="list" aria-label="What differs">
      <p class="muted head">{changed.length === 0 ? "The two trees are the same." : `${changed.length} file${changed.length === 1 ? "" : "s"} differ from ${named(against)} to ${named(ref)}`}</p>
      <ul>
        {#each changed as c (c.path)}
          <li>
            <button class="row leaf mono" aria-current={c.path === change?.path ? "true" : undefined} onclick={() => narrow({ path: c.path })}>
              <span>{c.path}</span><span class="kind {c.kind}">{verb[c.kind]}</span>
            </button>
          </li>
        {/each}
      </ul>
    </section>
    {#if change}
      <section class="code" aria-label={change.path}>
        <p class="head"><span class="mono">{change.path}</span>{#if counted}<span class="added">+{counted.added}</span><span class="removed">−{counted.removed}</span>{/if}{#if change.kind === "modified" && change.before.mode !== change.after.mode}<span class="muted">mode {change.before.mode} to {change.after.mode}</span>{/if}</p>
        {#if !sides}
          <p class="muted" role="status">Reading both sides.</p>
        {:else if diffed}
          {#each diffed as hunk, i (i)}
            <ol class="diff mono">
              {#each hunk.lines as line, j (j)}
                <li class={line.kind}><span class="number">{line.before ?? ""}</span><span class="number">{line.after ?? ""}</span><span class="sign" aria-hidden="true">{line.kind === "added" ? "+" : line.kind === "removed" ? "−" : " "}</span><span class="text">{line.text}</span></li>
              {/each}
            </ol>
          {:else}
            <p class="muted">The bytes are the same; only the mode changed.</p>
          {/each}
        {:else}
          <p class="muted">Not drawn: {sides.before && "not" in sides.before ? sides.before.not : sides.after && "not" in sides.after ? sides.after.not : ""}.</p>
        {/if}
      </section>
    {/if}
  </div>
{:else}
  <div class="columns">
    <section class="list" aria-label="Files at {ref}">
      <p class="muted head">{files.length} file{files.length === 1 ? "" : "s"} at <span class="mono">{ref}</span></p>
      <ul class="tree">{@render branch(nodes, 0)}</ul>
    </section>
    {#if entry}
      <section class="code" aria-label={entry.path}>
        <p class="head">
          <span class="mono">{entry.path}</span>
          <span class="faint">{sizeOf(entry.size)} · mode {entry.mode}</span>
          <span class="faint mono" title="SHA-256">{entry.sha256.slice(0, 12)}</span>
          <a class="right" href={download} download={entry.path.split("/").pop()}>Download</a>
        </p>
        {#if !content}
          <p class="muted" role="status">Reading {entry.path}.</p>
        {:else if "not" in content}
          <p class="muted">Not drawn: {content.not}.</p>
        {:else}
          {#if problems}
            <div class="checked" role="status">
              {#if problems.length === 0}
                <p class="good"><Icon name="state-succeeded" size={14} />Valid against <span class="mono">workflow.schema.json</span></p>
              {:else}
                <p class="bad"><Icon name="state-failed" size={14} />{problems.length} problem{problems.length === 1 ? "" : "s"} against <span class="mono">workflow.schema.json</span></p>
                <ul>
                  {#each problems as p, i (i)}<li><span class="mono faint">line {p.line}</span>{#if p.at}<span class="mono">{p.at}</span>{/if}<span>{p.message}</span></li>{/each}
                </ul>
              {/if}
            </div>
          {/if}
          <ol class="file mono" aria-label="{entry.path} at {ref}">
            {#each lines as line, i (i)}<li class:wrong={wrong.has(i + 1)}><span class="number">{i + 1}</span><span class="text">{line}</span></li>{/each}
          </ol>
        {/if}
      </section>
    {/if}
  </div>
  {#if listing && history.some((h) => h.commit === listing!.commit)}
    {@const h = history.find((x) => x.commit === listing!.commit)!}
    <p class="faint commit">{h.subject ?? ""}, by {h.author?.name ?? "somebody"} <time datetime={h.authored_at}>{h.authored_at ? clock(h.authored_at, now) : ""}</time></p>
  {/if}
{/if}

<style>
  .bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4) calc(var(--unit) * 8);
    margin-bottom: calc(var(--unit) * 7);
    font-size: var(--type-control-size);
  }

  .pick {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .pick input {
    width: 220px;
    height: 29px;
    padding: 0 calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--sunken);
    color: var(--text);
    font-size: var(--type-identifier-size-min);
  }

  .right {
    margin-left: auto;
  }

  .columns {
    display: grid;
    grid-template-columns: minmax(240px, 0.36fr) minmax(0, 1fr);
    gap: calc(var(--unit) * 7);
    align-items: start;
  }

  .list,
  .code {
    min-width: 0;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-pane);
    background: var(--surface);
  }

  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 5);
    margin: 0;
    padding: calc(var(--unit) * 5) calc(var(--unit) * 6);
    border-bottom: var(--border-hairline) solid var(--line);
    font-size: var(--type-control-size);
  }

  ul {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .list > ul {
    padding: calc(var(--unit) * 2) 0;
  }

  .row {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    width: 100%;
    padding: calc(var(--unit) * 2) calc(var(--unit) * 6);
    border: none;
    border-left: 2px solid transparent;
    background: none;
    color: var(--text);
    font-size: var(--type-identifier-size-min);
    text-align: left;
    cursor: pointer;
  }

  .row:hover {
    background: var(--raised);
  }

  .row[aria-current="true"] {
    border-left-color: var(--accent);
    background: var(--accentDim);
  }

  .row.folder {
    font-size: var(--type-control-size);
  }

  .caret {
    width: 10px;
    color: var(--faint);
  }

  .size,
  .kind {
    margin-left: auto;
    font-size: 11px;
  }

  .kind.added,
  .added {
    color: var(--succeeded);
  }

  .kind.removed,
  .removed {
    color: var(--failed);
  }

  .kind.modified {
    color: var(--waiting);
  }

  .file,
  .diff {
    margin: 0;
    padding: calc(var(--unit) * 3) 0;
    overflow: auto;
    background: var(--sunken);
    font-size: 12.5px;
    line-height: 1.6;
    list-style: none;
  }

  .file {
    max-height: 72vh;
    border-radius: 0 0 var(--radius-pane) var(--radius-pane);
  }

  .diff + .diff {
    border-top: var(--border-hairline) dashed var(--lineStrong);
  }

  .file li {
    display: grid;
    grid-template-columns: 52px 1fr;
  }

  .diff li {
    display: grid;
    grid-template-columns: 44px 44px 16px 1fr;
  }

  .diff li.added {
    background: var(--succeededFill);
  }

  .diff li.removed {
    background: var(--failedFill);
  }

  .number {
    padding-right: calc(var(--unit) * 4);
    color: var(--faint);
    text-align: right;
    user-select: none;
  }

  .sign {
    color: var(--muted);
  }

  .text {
    padding-right: calc(var(--unit) * 6);
    white-space: pre;
  }

  .file li.wrong {
    border-left: 2px solid var(--failed);
    background: var(--failedFill);
  }

  .checked {
    padding: calc(var(--unit) * 4) calc(var(--unit) * 6);
    border-bottom: var(--border-hairline) solid var(--line);
    font-size: var(--type-control-size);
  }

  .checked p {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    margin: 0;
  }

  .checked .good {
    color: var(--succeeded);
  }

  .checked .bad {
    color: var(--failed);
  }

  .checked ul {
    margin-top: calc(var(--unit) * 3);
  }

  .checked li {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 4);
    padding: 2px 0;
  }

  .commit {
    margin-top: calc(var(--unit) * 5);
    font-size: var(--type-control-size);
  }

  .refused {
    color: var(--failed);
  }
</style>
