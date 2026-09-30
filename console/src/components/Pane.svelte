<script lang="ts">
  import type { Snippet } from "svelte";

  // A pane: a block of the screen on the palette's surface, bordered with rounded corners and titled in
  // its top border, with what it counts at the other end of that border. The one in focus is bordered
  // in the accent, as a selection is.
  let {
    title,
    aside = "",
    focused = false,
    children,
    label,
  }: { title: string; aside?: string; focused?: boolean; children: Snippet; label?: string } = $props();
</script>

<section class="pane" class:focused aria-label={label ?? title}>
  <header>
    <h2>{title}</h2>
    {#if aside}<span class="aside">{aside}</span>{/if}
  </header>
  <div class="body">
    {@render children()}
  </div>
</section>

<style>
  .pane {
    position: relative;
    display: flex;
    flex-direction: column;
    min-height: 0;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-pane);
    background: var(--surface);
  }

  .pane.focused {
    border-color: var(--accent);
  }

  header {
    position: absolute;
    top: -0.75em;
    left: calc(var(--padding-panel) - 6px);
    right: calc(var(--padding-panel) - 6px);
    display: flex;
    justify-content: space-between;
    pointer-events: none;
    line-height: 1.5;
  }

  /* Each end of the title cuts the border it sits on: the ground above the line, the pane's surface
     below it. */
  h2,
  .aside {
    padding: 0 6px;
    background: linear-gradient(to bottom, var(--bg) 50%, var(--surface) 50%);
  }

  h2 {
    margin: 0;
    font-family: var(--type-sectionTitle-font);
    font-size: var(--type-sectionTitle-size);
    font-weight: var(--type-sectionTitle-weight);
  }

  .aside {
    color: var(--faint);
    font-family: var(--type-identifier-font);
    font-size: 11.5px;
    align-self: center;
  }

  .body {
    display: flex;
    flex-direction: column;
    min-height: 0;
    flex: 1;
    padding: calc(var(--padding-panel) + 4px) var(--padding-panel) var(--padding-panel);
  }
</style>
