<script lang="ts">
  import type { Me } from "../api/client";
  import { clock } from "../lib/format";
  import { dismissible, said } from "../lib/notifications";
  import Icon from "./Icon.svelte";
  import Popover from "./Popover.svelte";

  // The caller's notifications, an entry of the sidebar under the home: how many wait beside its word,
  // or on its bell where the sidebar is folded, and opened, each one's words, its moment and a way to
  // dismiss it, where the API lets it be dismissed.
  let { me, folded = false, ondismiss }: { me: Me; folded?: boolean; ondismiss: (id: string) => void } = $props();

  const waiting = $derived(me.notifications.length);
</script>

<Popover label={waiting === 0 ? "Notifications, none" : `Notifications, ${waiting}`} align="start" width={340} block>
  {#snippet button()}
    <span class="entry" class:folded>
      <span class="bell">
        <Icon name="control-notifications" />
        {#if folded && waiting > 0}<span class="count over">{waiting}</span>{/if}
      </span>
      {#if !folded}
        <span class="label">Notifications</span>
        {#if waiting > 0}<span class="count">{waiting}</span>{/if}
      {/if}
    </span>
  {/snippet}
  {#snippet children()}
    {#if waiting === 0}
      <p class="empty">Nothing to tell you.</p>
    {:else}
      <ul class="notifications">
        {#each me.notifications as notice (notice.id)}
          <li>
            <span class="said">{said(notice)}</span>
            <time class="faint" datetime={notice.at} title={notice.at}>{clock(notice.at, Date.now())}</time>
            {#if dismissible(notice)}<button class="control" onclick={() => ondismiss(notice.id)}>Dismiss</button>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  {/snippet}
</Popover>

<style>
  /* Drawn as an entry of the sidebar is. */
  .entry {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    width: 100%;
    height: 34px;
    padding: 0 calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    color: var(--muted);
    font-family: var(--type-navigation-font);
    font-size: var(--type-navigation-size);
    font-weight: var(--type-navigation-weight);
  }

  .entry.folded {
    justify-content: center;
    padding: 0;
  }

  .entry:hover {
    background: var(--raised);
    color: var(--text);
  }

  .bell {
    position: relative;
    display: inline-flex;
  }

  .count {
    min-width: 18px;
    height: 18px;
    margin-left: auto;
    padding: 0 calc(var(--unit) * 2);
    border-radius: var(--radius-round);
    background: var(--waiting);
    color: var(--bg);
    font-size: 11px;
    font-weight: 700;
    line-height: 18px;
    text-align: center;
  }

  /* Folded, the count sits on the bell's corner, as a badge. */
  .count.over {
    position: absolute;
    top: -7px;
    right: -8px;
    min-width: 14px;
    height: 14px;
    padding: 0 3px;
    font-size: 10px;
    line-height: 14px;
  }

  .empty {
    margin: calc(var(--unit) * 4);
    color: var(--muted);
  }

  .notifications {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .notifications li {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 2px calc(var(--unit) * 4);
    padding: calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
  }

  .notifications li:last-child {
    border-bottom: none;
  }

  .notifications .said {
    --leading: 1.45;
  }

  .notifications time {
    grid-column: 1;
    font-size: var(--type-control-size);
  }

  .notifications button {
    grid-row: 1 / span 2;
    grid-column: 2;
    align-self: center;
  }
</style>
