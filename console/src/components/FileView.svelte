<script lang="ts">
  import { stepAt, type Block } from "../lib/yaml-blocks";

  // A workflow file as its version holds it, with line numbers, the block of the step chosen
  // highlighted and brought into view, and a click on a line inside a step's block choosing that
  // step, so that the graph and the file select each other. Read only: editing is the editor's.
  let { text, found, selected, onselect }: { text: string; found: Map<string, Block>; selected: string | undefined; onselect: (step: string) => void } = $props();

  const lines = $derived(text.replace(/\n$/, "").split("\n"));
  const block = $derived(selected ? found.get(selected) : undefined);
  let holder: HTMLOListElement | undefined = $state();

  // Each line cut into its indentation, its key, and the rest, which is what is coloured: a key in
  // the accent, a comment faint, the rest as written.
  function parts(line: string): { lead: string; key?: string; rest: string; comment?: string } {
    const hash = line.search(/(^|\s)#/);
    const code = hash >= 0 ? line.slice(0, hash) : line;
    const comment = hash >= 0 ? line.slice(hash) : undefined;
    const m = /^(\s*-?\s*)([^\s:#"'][^:#]*?|"[^"]*"|'[^']*')(:)(\s|$)/.exec(code);
    if (!m) return { lead: "", rest: code, comment };
    return { lead: m[1]!, key: m[2]! + m[3]!, rest: code.slice(m[1]!.length + m[2]!.length + 1), comment };
  }

  $effect(() => {
    if (!block || !holder) return;
    // The block's first line a few lines from the top of the file's own scroll, and the page left
    // where it is.
    const first = holder.querySelector<HTMLElement>(`[data-line="${block.start}"]`);
    if (first) holder.scrollTop = Math.max(0, first.offsetTop - 3 * first.offsetHeight);
  });
</script>

<ol class="file code" bind:this={holder} aria-label="agentiik.yaml">
  {#each lines as line, i (i)}
    {@const n = i + 1}
    {@const p = parts(line)}
    {@const inBlock = block !== undefined && n >= block.start && n <= block.end}
    <li data-line={n} class:chosen={inBlock}>
      <button class="number" tabindex="-1" aria-hidden="true" onclick={() => { const s = stepAt(found, n); if (s) onselect(s); }}>{n}</button>
      <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
      <span class="text" onclick={() => { const s = stepAt(found, n); if (s) onselect(s); }}>{p.lead}{#if p.key}<span class="key">{p.key}</span>{/if}{p.rest}{#if p.comment}<span class="comment">{p.comment}</span>{/if}</span>
    </li>
  {/each}
</ol>

<style>
  .file {
    position: relative;
    max-height: 72vh;
    margin: 0;
    padding: calc(var(--unit) * 3) 0;
    overflow: auto;
    border-radius: var(--radius-control);
    background: var(--sunken);
    font-size: 12.5px;
    line-height: 1.6;
    list-style: none;
  }

  li {
    display: grid;
    grid-template-columns: 44px 1fr;
    border-left: 2px solid transparent;
  }

  li.chosen {
    border-left-color: var(--accent);
    background: var(--accentDim);
  }

  .number {
    padding: 0 calc(var(--unit) * 4) 0 0;
    border: none;
    background: none;
    color: var(--faint);
    font: inherit;
    text-align: right;
    cursor: pointer;
  }

  .text {
    padding-right: calc(var(--unit) * 5);
    white-space: pre;
    cursor: pointer;
  }

  .key {
    color: var(--accent);
  }

  .comment {
    color: var(--faint);
  }
</style>
