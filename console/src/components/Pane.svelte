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

  /* The title and what the pane counts read along one baseline, set in two sizes as they are, and the
     line they make is centred in the row: a wrapping container centres its line with align-content,
     where a single line would have each item centred on its own. */
  header {
    display: flex;
    flex-wrap: wrap;
    align-content: center;
    align-items: baseline;
    justify-content: space-between;
    gap: calc(var(--unit) * 6);
    min-height: 46px;
    padding: 0 var(--padding-panel) 0 calc(var(--padding-panel) + 2px);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
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

  /* A table wider than the window scrolls inside its pane, rather than the page beside it. */
  .body {
    display: flex;
    flex-direction: column;
    min-height: 0;
    flex: 1;
    overflow-x: auto;
    padding: var(--padding-panel) var(--padding-panel) var(--padding-panel) calc(var(--padding-panel) + 2px);
  }

  /* A table runs from one edge of its pane to the other, its hairlines and the row under the pointer
     across the whole pane, and its outer columns keep the pane's gutter, so that the text of its first
     column starts under the pane's title as every paragraph of the pane does. A table that scrolls
     sideways runs out with the box it scrolls in. A grid, cells drawn apart rather than rows, keeps
     its own edges. */
  .body > :global(table:not(.tiled)),
  .body > :global(.scroll),
  .body > :global(details) > :global(table:not(.tiled)) {
    width: calc(100% + 2 * var(--padding-panel) + 2px);
    max-width: none;
    margin-left: calc(-1 * (var(--padding-panel) + 2px));
    margin-right: calc(-1 * var(--padding-panel));
  }

  .body > :global(.scroll) > :global(table) {
    width: 100%;
  }

  .body > :global(table:not(.tiled)) :global(:is(th, td):first-child),
  .body > :global(.scroll) :global(:is(th, td):first-child),
  .body > :global(details) > :global(table:not(.tiled)) :global(:is(th, td):first-child) {
    padding-left: calc(var(--padding-panel) + 2px);
  }

  .body > :global(table:not(.tiled)) :global(:is(th, td):last-child),
  .body > :global(.scroll) :global(:is(th, td):last-child),
  .body > :global(details) > :global(table:not(.tiled)) :global(:is(th, td):last-child) {
    padding-right: var(--padding-panel);
  }
</style>
