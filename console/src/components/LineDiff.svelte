<script lang="ts">
  import type { Hunk } from "../lib/tree";

  // A change line by line, as a review reads it: each hunk the lines removed and added with those
  // around them, each numbered in the file it is in, a dashed hairline between two hunks. The files
  // compared at two refs and, in the editor, a change a moved branch refused are drawn with it.
  let { hunks }: { hunks: Hunk[] } = $props();
</script>

{#each hunks as hunk, i (i)}
  <ol class="diff code">
    {#each hunk.lines as line, j (j)}
      <li class={line.kind}><span class="number">{line.before ?? ""}</span><span class="number">{line.after ?? ""}</span><span class="sign" aria-hidden="true">{line.kind === "added" ? "+" : line.kind === "removed" ? "−" : " "}</span><span class="text">{line.text}</span></li>
    {/each}
  </ol>
{/each}

<style>
  .diff {
    margin: 0;
    padding: calc(var(--unit) * 3) 0;
    overflow: auto;
    background: var(--sunken);
    font-size: 12.5px;
    --leading: 1.6;
    list-style: none;
  }

  .diff + .diff {
    border-top: var(--border-hairline) dashed var(--lineStrong);
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
</style>
