<script lang="ts">
  import type { Snippet } from "svelte";
  import type { Explained } from "../lib/problem";
  import Icon from "./Icon.svelte";

  // What an act just did, or why the API refused it, said at the foot of the screen over what is there
  // rather than above it, where it would push the page down as it came and pull it up as it went. What
  // was done goes by itself after a while; a refusal stays until it is put away, since it may need
  // reading twice.
  // A failure is given as explained, what failed first, why and what to do after it, and the server's
  // own answer in small type.
  let { kind = "said", children, explained, ondismiss }: { kind?: "said" | "problem"; children?: Snippet; explained?: Explained | null; ondismiss?: () => void } = $props();

  let shown = $state(true);

  function dismiss() {
    shown = false;
    ondismiss?.();
  }

  $effect(() => {
    if (kind !== "said") return;
    const timer = setTimeout(dismiss, 8000);
    return () => clearTimeout(timer);
  });
</script>

{#if shown}
  <div class="notice {kind}">
    <span class="icon"><Icon name={kind === "problem" ? "state-failed" : "state-succeeded"} size={16} /></span>
    {#if explained}
      <div class="told" role="alert">
        <p class="what">{explained.what}</p>
        <p>{explained.next ? `${explained.why} ${explained.next}` : explained.why}</p>
        {#if explained.detail}<p class="detail">{explained.detail}</p>{/if}
      </div>
    {:else}
      <p role={kind === "problem" ? "alert" : "status"}>{@render children?.()}</p>
    {/if}
    <button class="dismiss" aria-label="Put away" onclick={dismiss}><Icon name="control-close" size={14} /></button>
  </div>
{/if}

<style>
  .notice {
    position: fixed;
    right: calc(var(--padding-page) + 8px);
    bottom: calc(var(--padding-page) + 8px);
    z-index: 40;
    display: flex;
    align-items: flex-start;
    gap: calc(var(--unit) * 4);
    width: min(440px, calc(100vw - 2 * var(--padding-page)));
    padding: calc(var(--unit) * 4) calc(var(--unit) * 3) calc(var(--unit) * 4) calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--lineStrong);
    border-left: 3px solid var(--succeeded);
    border-radius: var(--radius-card);
    background: var(--surface);
    box-shadow: 0 8px 24px rgb(0 0 0 / 0.14);
    font-size: var(--type-control-size);
  }

  .notice.problem {
    border-left-color: var(--failed);
  }

  .icon {
    display: inline-flex;
    padding-top: 2px;
    color: var(--succeeded);
  }

  .problem .icon {
    color: var(--failed);
  }

  p {
    flex: 1;
    margin: 0;
  }

  .told {
    display: grid;
    flex: 1;
    gap: calc(var(--unit) * 2);
  }

  .what {
    font-weight: 600;
  }

  .detail {
    color: var(--faint);
    font-size: var(--type-identifier-size-min);
    overflow-wrap: anywhere;
  }

  .dismiss {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    border: none;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    cursor: pointer;
  }

  .dismiss:hover {
    background: var(--raised);
    color: var(--text);
  }
</style>
