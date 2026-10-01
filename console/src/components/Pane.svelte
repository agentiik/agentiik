<script lang="ts">
  import type { Snippet } from "svelte";

  // A pane: a card on the palette's surface, its title on the row above its content with what it counts
  // and what can be done to it at the other end. The one in focus is bordered in the accent, as a
  // selection is.
  let {
    title,
    aside = "",
    focused = false,
    children,
    actions,
    label,
  }: { title: string; aside?: string; focused?: boolean; children: Snippet; actions?: Snippet; label?: string } = $props();
</script>

<section class="pane" class:focused aria-label={label ?? title}>
  <header class:none={!title && !aside && !actions}>
    <h2>{title}</h2>
    {#if aside || actions}
      <span class="end">
        {#if aside}<span class="aside">{aside}</span>{/if}
        {#if actions}{@render actions()}{/if}
      </span>
    {/if}
  </header>
  <div class="body">
    {@render children()}
  </div>
</section>

<style>
  /* A pane is a card: its title and what it counts on a row of their own above a hairline, rather than
     set into the border as agk console sets them, which a browser has no need to save the room of. */
  .pane {
    position: relative;
    display: flex;
    flex-direction: column;
    min-height: 0;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-card);
    background: var(--surface);
  }

  .pane.focused {
    border-color: var(--accentLine);
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: calc(var(--unit) * 6);
    min-height: 46px;
    padding: 0 var(--padding-panel) 0 calc(var(--padding-panel) + 2px);
    border-bottom: var(--border-hairline) solid var(--line);
  }

  /* A pane with nothing to say above its content, a list under its screen's own head, draws no row. */
  header.none {
    display: none;
  }

  h2 {
    margin: 0;
    font-family: var(--type-sectionTitle-font);
    font-size: var(--type-sectionTitle-size);
    font-weight: var(--type-sectionTitle-weight);
  }

  .end {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
  }

  .aside {
    color: var(--faint);
    font-size: 13px;
    font-variant-numeric: tabular-nums;
    text-align: right;
  }

  .body {
    display: flex;
    flex-direction: column;
    min-height: 0;
    flex: 1;
    padding: var(--padding-panel) var(--padding-panel) var(--padding-panel) calc(var(--padding-panel) + 2px);
  }
</style>
