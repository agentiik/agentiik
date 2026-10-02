<script lang="ts">
  import { tick } from "svelte";
  import type { API, Namespace } from "../api/client";
  import { chosenLast, choose, narrowed, opening, wholeRun, type Entry, type Found } from "../lib/palette";
  import type { Place } from "../lib/place.svelte";
  import { runAt } from "../lib/route";
  import Icon from "./Icon.svelte";
  import StatePill from "./StatePill.svelte";

  // The palette, over the screen: what entries holds, the views, the namespaces and the keys of the
  // screen drawn, with each namespace's workflows and the latest runs read as it opens, narrowed as
  // letters are typed (lib/palette). ↑ and ↓ move among them, enter or a click opens one, and esc
  // closes it, the focus going back where it was.
  let {
    api,
    place,
    open = $bindable(false),
    entries,
    reads,
  }: { api: API; place: Place; open?: boolean; entries: Entry[]; reads: Namespace[] } = $props();

  let typed = $state("");
  let selected = $state(0);
  let field = $state<HTMLInputElement | undefined>();
  let list = $state<HTMLElement | undefined>();
  let before: Element | null = null;
  let last = $state<string[]>([]);

  // What is read as the palette opens: each namespace's workflows and the latest runs, and a run
  // named whole that the latest do not hold.
  let workflows = $state<Entry[]>([]);
  let runs = $state<Entry[]>([]);
  let named = $state<Entry | undefined>();

  const runEntry = (r: { run: string; namespace: string; workflow: string; state: string }): Entry => ({
    id: `run:${r.run}`,
    kind: "run",
    label: r.run,
    detail: `${r.namespace}/${r.workflow}`,
    state: r.state,
    to: runAt(r.namespace, r.workflow, r.run),
  });

  async function readAll() {
    const [listed, latest] = await Promise.all([
      Promise.all(
        reads.map(async (n) => {
          const { data } = await api.GET("/api/v1/{ns}/workflows", { params: { path: { ns: n.name } } });
          return (data?.workflows ?? []).map(
            (w): Entry => ({ id: `workflow:${n.name}/${w.name}`, kind: "workflow", label: w.name, detail: n.name, to: { kind: "namespace", namespace: n.name, view: "workflows", workflow: w.name } }),
          );
        }),
      ),
      api.GET("/api/v1/runs", { params: { query: { limit: 50 } } }).then(({ data }) => data?.runs ?? []),
    ]);
    if (!open) return;
    workflows = listed.flat();
    runs = latest.map(runEntry);
  }

  $effect(() => {
    if (!open) return;
    before = document.activeElement;
    typed = "";
    selected = 0;
    named = undefined;
    last = chosenLast(globalThis.localStorage);
    void readAll();
    tick().then(() => field?.focus());
    return () => {
      if (before instanceof HTMLElement && before.isConnected) before.focus();
    };
  });

  // A run's whole identifier typed is read on its own where the latest runs do not hold it, so that
  // a run however old is opened from a link or a log.
  $effect(() => {
    const id = typed.trim().toUpperCase();
    if (!open || !wholeRun.test(id) || runs.some((r) => r.label === id)) {
      named = undefined;
      return;
    }
    let current = true;
    api.GET("/api/v1/runs/{id}", { params: { path: { id } } }).then(({ data }) => {
      if (current && data) named = runEntry(data);
    });
    return () => {
      current = false;
    };
  });

  const everything = $derived([...entries.filter((e) => e.kind !== "key"), ...workflows, ...(named ? [named] : []), ...runs, ...entries.filter((e) => e.kind === "key")]);
  const shownAtMost = 60;

  // Before anything is typed, the entries chosen last, then every other by its kind; once typed, the
  // closest first, whatever their kind.
  const groups = $derived.by((): { title?: string; found: Found[] }[] => {
    if (typed.trim() === "") {
      const { recent, rest } = opening(everything, last);
      const titled: [Entry["kind"], string][] = [
        ["view", "Views"],
        ["namespace", "Namespaces"],
        ["workflow", "Workflows"],
        ["run", "Runs"],
        ["key", "Keys"],
      ];
      const as = (e: Entry): Found => ({ entry: e, score: 0, at: [] });
      return [
        ...(recent.length > 0 ? [{ title: "Chosen last", found: recent.map(as) }] : []),
        ...titled.map(([kind, title]) => ({ title, found: rest.filter((e) => e.kind === kind).map(as) })).filter((g) => g.found.length > 0),
      ];
    }
    return [{ found: narrowed(everything, typed).slice(0, shownAtMost) }];
  });
  const flat = $derived(groups.flatMap((g) => g.found));

  $effect(() => {
    void typed;
    selected = 0;
  });

  $effect(() => {
    const i = selected;
    tick().then(() => list?.querySelector<HTMLElement>(`[data-index="${i}"]`)?.scrollIntoView?.({ block: "nearest" }));
  });

  function close() {
    open = false;
  }

  function pick(entry: Entry | undefined) {
    if (!entry) return;
    choose(globalThis.localStorage, entry.id);
    close();
    if (entry.to) place.go(entry.to);
    // A key does what it does once the focus is back where the palette found it.
    else if (entry.does) tick().then(entry.does);
  }

  function keydown(e: KeyboardEvent) {
    e.stopPropagation();
    if (e.key === "Escape") {
      e.preventDefault();
      close();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      selected = Math.min(flat.length - 1, selected + 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      selected = Math.max(0, selected - 1);
    } else if (e.key === "Enter") {
      e.preventDefault();
      pick(flat[selected]?.entry);
    } else if (e.key === "Tab") {
      e.preventDefault();
    }
  }

  // The letters of a label the words typed matched, drawn apart from the others.
  function pieces(label: string, at: number[]): { text: string; hit: boolean }[] {
    const hits = new Set(at);
    const out: { text: string; hit: boolean }[] = [];
    [...label].forEach((ch, i) => {
      const hit = hits.has(i);
      const prev = out[out.length - 1];
      if (prev && prev.hit === hit) prev.text += ch;
      else out.push({ text: ch, hit });
    });
    return out;
  }

  const icons: Record<Entry["kind"], string> = { view: "control-open", namespace: "control-namespaces", workflow: "control-workflows", run: "control-runs", key: "control-next" };
  const words: Record<Entry["kind"], string> = { view: "View", namespace: "Namespace", workflow: "Workflow", run: "Run", key: "Key" };
  const indexOf = (f: Found) => flat.indexOf(f);
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={(e) => e.target === e.currentTarget && close()}>
    <div class="palette" role="dialog" aria-modal="true" aria-label="Search" tabindex="-1" onkeydown={keydown}>
      <div class="typed">
        <Icon name="control-search" />
        <input
          bind:this={field}
          bind:value={typed}
          type="text"
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={flat.length > 0 ? `palette-${selected}` : undefined}
          aria-label="Search"
          placeholder="Search"
          autocomplete="off"
          spellcheck="false"
        />
        <kbd>esc</kbd>
      </div>
      <div class="list" id="palette-list" role="listbox" aria-label="Found" bind:this={list}>
        {#each groups as group, g (group.title ?? g)}
          {#if group.title}<p class="group" role="presentation">{group.title}</p>{/if}
          {#each group.found as f (f.entry.id)}
            {@const i = indexOf(f)}
            <!-- svelte-ignore a11y_click_events_have_key_events -->
            <div class="entry" class:on={i === selected} id="palette-{i}" data-index={i} role="option" aria-selected={i === selected} tabindex="-1" onclick={() => pick(f.entry)} onmousemove={() => (selected = i)}>
              <span class="icon"><Icon name={f.entry.icon ?? icons[f.entry.kind]} /></span>
              <span class="label" class:code={f.entry.kind === "run"}>
                {#each pieces(f.entry.label, f.at) as p, n (n)}{#if p.hit}<mark>{p.text}</mark>{:else}{p.text}{/if}{/each}
              </span>
              {#if f.entry.detail}<span class="detail">{f.entry.detail}</span>{/if}
              <span class="end">
                {#if f.entry.state}<StatePill state={f.entry.state} live={false} />
                {:else if f.entry.key}<kbd>{f.entry.key}</kbd>
                {:else}<span class="kind">{words[f.entry.kind]}</span>{/if}
              </span>
            </div>
          {/each}
        {/each}
        {#if flat.length === 0}<p class="none">Nothing matches.</p>{/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 60;
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding: 12vh 16px 16px;
    background: color-mix(in srgb, var(--bg) 55%, transparent);
    backdrop-filter: blur(1px);
  }

  .palette {
    display: flex;
    flex-direction: column;
    width: 640px;
    max-width: 100%;
    max-height: 70vh;
    overflow: hidden;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-card);
    background: var(--surface);
    box-shadow: 0 12px 32px rgb(0 0 0 / 0.18);
  }

  .palette:focus {
    outline: none;
  }

  .typed {
    display: flex;
    flex: none;
    align-items: center;
    gap: calc(var(--unit) * 4);
    height: 50px;
    padding: 0 var(--padding-panel);
    color: var(--muted);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
  }

  .typed input {
    flex: 1;
    min-width: 0;
    height: 100%;
    padding: 0;
    border: none;
    background: none;
    color: var(--text);
    font: inherit;
    font-size: var(--type-sectionTitle-size);
    outline: none;
  }

  .typed input::placeholder {
    color: var(--faint);
  }

  .list {
    overflow-y: auto;
    padding: calc(var(--unit) * 2);
  }

  .group {
    margin: calc(var(--unit) * 3) calc(var(--unit) * 4) calc(var(--unit) * 1);
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
  }

  .entry {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    height: 36px;
    padding: 0 calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    color: var(--text);
    cursor: pointer;
  }

  .entry.on {
    background: var(--accentDim);
  }

  .icon {
    display: inline-flex;
    flex: none;
    color: var(--muted);
  }

  .entry.on .icon {
    color: var(--accent);
  }

  .label {
    flex: none;
    max-width: 60%;
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
  }

  .label mark {
    background: none;
    color: var(--accent);
    font-weight: 600;
  }

  .detail {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    color: var(--muted);
    white-space: nowrap;
    text-overflow: ellipsis;
  }

  .end {
    display: inline-flex;
    flex: none;
    margin-left: auto;
  }

  .kind {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
  }

  /* As the key line draws a key. */
  kbd {
    min-width: 18px;
    padding: 0 calc(var(--unit) * 2);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-chip);
    background: var(--raised);
    color: var(--text);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
    line-height: 18px;
    text-align: center;
  }

  .none {
    margin: 0;
    padding: calc(var(--unit) * 5) calc(var(--unit) * 4);
    color: var(--muted);
  }
</style>
