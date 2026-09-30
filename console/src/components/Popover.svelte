<script lang="ts">
  import type { Snippet } from "svelte";

  // A menu that opens under its button and closes on escape, on a click outside it, or once something
  // in it is chosen. The button says whether it is open, and focus returns to it on closing.
  let {
    label,
    button,
    children,
    align = "start",
    width = 260,
  }: { label: string; button: Snippet; children: Snippet<[() => void]>; align?: "start" | "end"; width?: number } = $props();

  let open = $state(false);
  let root: HTMLElement | undefined = $state();
  let trigger: HTMLButtonElement | undefined = $state();

  function close() {
    open = false;
    trigger?.focus();
  }

  function outside(event: MouseEvent) {
    if (open && root && !root.contains(event.target as Node)) {
      open = false;
    }
  }

  function key(event: KeyboardEvent) {
    if (open && event.key === "Escape") {
      event.stopPropagation();
      close();
    }
  }
</script>

<svelte:window onclick={outside} />

<div class="popover" bind:this={root} onkeydown={key} role="presentation">
  <button bind:this={trigger} class="trigger" aria-haspopup="true" aria-expanded={open} aria-label={label} onclick={() => (open = !open)}>
    {@render button()}
  </button>
  {#if open}
    <div class="menu {align}" style:width="{width}px">
      {@render children(close)}
    </div>
  {/if}
</div>

<style>
  .popover {
    position: relative;
    display: inline-flex;
  }

  .trigger {
    display: inline-flex;
    align-items: center;
    padding: 0;
    border: none;
    background: none;
    cursor: pointer;
  }

  .menu {
    position: absolute;
    top: calc(100% + 6px);
    z-index: 20;
    padding: calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-card);
    background: var(--raised);
    box-shadow: 0 8px 24px rgb(0 0 0 / 0.12);
  }

  .menu.start {
    left: 0;
  }

  .menu.end {
    right: 0;
  }
</style>
