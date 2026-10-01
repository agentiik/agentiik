<script lang="ts">
  import type { Snippet } from "svelte";

  // A menu that opens under its button, or over it at the foot of the window, and closes on escape, on a click outside it, or once something
  // in it is chosen. The button says whether it is open, and focus returns to it on closing.
  let {
    label,
    button,
    children,
    align = "start",
    width = 260,
    block = false,
    side = "bottom",
  }: { label: string; button: Snippet; children: Snippet<[() => void]>; align?: "start" | "end"; width?: number; block?: boolean; side?: "top" | "bottom" } = $props();

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

  // layered is whether the browser has a top layer to draw the menu in; one without it, and the
  // test's document among them, draws the menu fixed over the page instead.
  const layered = typeof HTMLElement !== "undefined" && "showPopover" in HTMLElement.prototype;

  // shown draws the menu in the browser's top layer, above every box of the page and clipped by
  // none of them, the sidebar that scrolls its own list among them, and places it from its button
  // again whenever the window is resized or anything under it scrolls.
  function shown(menu: HTMLElement) {
    const place = () => {
      if (!trigger) return;
      const at = trigger.getBoundingClientRect();
      const page = document.documentElement;
      const gap = 6;
      if (align === "start") {
        menu.style.left = `${Math.round(Math.max(gap, Math.min(at.left, page.clientWidth - width - gap)))}px`;
      } else {
        menu.style.right = `${Math.round(Math.max(gap, page.clientWidth - at.right))}px`;
      }
      if (side === "bottom") {
        menu.style.top = `${Math.round(at.bottom + gap)}px`;
      } else {
        menu.style.bottom = `${Math.round(page.clientHeight - at.top + gap)}px`;
      }
    };
    place();
    if (layered) menu.showPopover();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }

  function key(event: KeyboardEvent) {
    if (open && event.key === "Escape") {
      event.stopPropagation();
      close();
    }
  }
</script>

<svelte:window onclick={outside} />

<div class="popover" class:block bind:this={root} onkeydown={key} role="presentation">
  <button bind:this={trigger} class="trigger" aria-haspopup="true" aria-expanded={open} aria-label={label} onclick={() => (open = !open)}>
    {@render button()}
  </button>
  {#if open}
    <div class="menu" popover={layered ? "manual" : undefined} style:width="{width}px" {@attach shown}>
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

  .block,
  .block .trigger {
    display: flex;
    width: 100%;
  }

  /* The top layer's own placement, the middle of the window, is undone: the menu sits where shown
     puts it, beside its button. */
  .menu {
    position: fixed;
    inset: auto;
    z-index: 70;
    margin: 0;
    padding: calc(var(--unit) * 3);
    overflow: visible;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-card);
    background: var(--raised);
    color: var(--text);
    box-shadow: 0 8px 24px rgb(0 0 0 / 0.12);
  }
</style>
