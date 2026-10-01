<script lang="ts">
  import type { Snippet } from "svelte";
  import { tick } from "svelte";
  import Icon from "./Icon.svelte";

  // A dialog, over the screen: what a screen creates or writes is asked here, opened from the button at
  // its head, rather than in a form under its list which the list, once read, would push down the page.
  // The keys pressed inside it are its own, esc closes it, and the focus goes back where it was.
  let {
    title,
    open = $bindable(false),
    width = 480,
    onclose,
    children,
  }: { title: string; open?: boolean; width?: number; onclose?: () => void; children: Snippet } = $props();

  const id = `dialog-${Math.random().toString(36).slice(2, 9)}`;
  let box = $state<HTMLElement | undefined>();
  let before: Element | null = null;

  const focusable = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

  $effect(() => {
    if (!open) return;
    before = document.activeElement;
    tick().then(() => {
      const first = box?.querySelector<HTMLElement>(".body " + focusable.split(", ").join(", .body ")) ?? box;
      first?.focus();
    });
    return () => {
      if (before instanceof HTMLElement && before.isConnected) before.focus();
    };
  });

  function close() {
    open = false;
    onclose?.();
  }

  function keydown(e: KeyboardEvent) {
    e.stopPropagation();
    if (e.key === "Escape") {
      e.preventDefault();
      close();
      return;
    }
    if (e.key !== "Tab" || !box) return;
    const all = [...box.querySelectorAll<HTMLElement>(focusable)];
    if (all.length === 0) return;
    const first = all[0]!;
    const last = all[all.length - 1]!;
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={(e) => e.target === e.currentTarget && close()}>
    <div class="dialog" role="dialog" aria-modal="true" aria-labelledby={id} tabindex="-1" style:width="{width}px" bind:this={box} onkeydown={keydown}>
      <header>
        <h2 {id}>{title}</h2>
        <button class="close" aria-label="Close" onclick={close}><Icon name="control-close" size={16} /></button>
      </header>
      <div class="body">
        {@render children()}
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 50;
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding: 12vh 16px 16px;
    overflow-y: auto;
    background: color-mix(in srgb, var(--bg) 55%, transparent);
    backdrop-filter: blur(1px);
  }

  .dialog {
    max-width: 100%;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-card);
    background: var(--surface);
    box-shadow: 0 12px 32px rgb(0 0 0 / 0.18);
  }

  .dialog:focus {
    outline: none;
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    min-height: 50px;
    padding: 0 calc(var(--unit) * 4) 0 var(--padding-panel);
    border-bottom: var(--border-hairline) solid var(--line);
  }

  h2 {
    margin: 0;
    font-family: var(--type-sectionTitle-font);
    font-size: var(--type-sectionTitle-size);
    font-weight: var(--type-sectionTitle-weight);
  }

  .close {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 30px;
    height: 30px;
    border: none;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    cursor: pointer;
  }

  .close:hover {
    background: var(--raised);
    color: var(--text);
  }

  .body {
    padding: var(--padding-panel);
  }
</style>
