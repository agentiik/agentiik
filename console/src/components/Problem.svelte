<script lang="ts">
  import type { Explained } from "../lib/problem";
  import Icon from "./Icon.svelte";

  // A failure told where it happened, in the place of what could not be shown or beside what could
  // not be done: what failed, why in everyday words and what to do, then the server's own answer in
  // small type for whoever has to look further, and Try again where trying again may help.
  let { explained, onretry }: { explained: Explained; onretry?: () => void } = $props();
</script>

<div class="problem" role="alert">
  <p class="what"><Icon name="state-failed" size={16} />{explained.what}</p>
  <p class="why">{explained.next ? `${explained.why} ${explained.next}` : explained.why}</p>
  {#if explained.detail}<p class="detail">{explained.detail}</p>{/if}
  {#if onretry && explained.transient}<p class="again"><button class="control" onclick={onretry}>Try again</button></p>{/if}
</div>

<style>
  .problem {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  p {
    margin: 0;
  }

  .what {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    color: var(--failed);
    font-weight: 600;
  }

  .why {
    color: var(--text);
  }

  .detail {
    color: var(--faint);
    font-size: var(--type-identifier-size-min);
    overflow-wrap: anywhere;
  }

  .again {
    margin-top: calc(var(--unit) * 2);
  }
</style>
