<script lang="ts">
  import type { Snippet } from "svelte";
  import { follow, type Place } from "../lib/place.svelte";
  import type { Tab } from "../lib/page";
  import Icon from "./Icon.svelte";

  // The head of a screen: what it is, as large as the page's title, how many it holds, what can be done
  // from it at the other end, and the screen's tabs below, each a link drawn the same whichever is
  // chosen, so that choosing one moves nothing.
  let {
    title,
    icon,
    count,
    place,
    tabs = [],
    subtitle,
    actions,
    code = false,
  }: { title: string; icon?: string; count?: number | string; place: Place; tabs?: Tab[]; subtitle?: Snippet; actions?: Snippet; code?: boolean } = $props();
</script>

<header class="page" class:tabbed={tabs.length > 0}>
  <div class="row">
    <div class="what">
      {#if icon}<span class="icon"><Icon name={icon} size={20} /></span>{/if}
      <h1 class:code>{title}</h1>
      {#if count !== undefined}<span class="count">{count}</span>{/if}
      {#if subtitle}<span class="subtitle">{@render subtitle()}</span>{/if}
    </div>
    {#if actions}<div class="actions">{@render actions()}</div>{/if}
  </div>
  {#if tabs.length > 0}
    <nav class="tabs" aria-label="{title}, what is shown">
      {#each tabs as t (t.label)}
        {#if t.to}
          <a class="tab" aria-current={t.current ? "page" : undefined} href={place.href(t.to) + (t.query ?? "")} onclick={t.onclick ? (e) => { e.preventDefault(); t.onclick?.(); } : follow(place, t.to)}>
            {#if t.icon}<Icon name={t.icon} size={15} />{/if}{t.label}{#if t.count !== undefined}<span class="badge">{t.count}</span>{/if}
          </a>
        {:else}
          <button class="tab" aria-pressed={t.current} onclick={() => t.onclick?.()}>
            {#if t.icon}<Icon name={t.icon} size={15} />{/if}{t.label}{#if t.count !== undefined}<span class="badge">{t.count}</span>{/if}
          </button>
        {/if}
      {/each}
    </nav>
  {/if}
</header>

<style>
  .page {
    margin: 0 0 calc(var(--unit) * 9);
  }

  .page.tabbed {
    border-bottom: var(--border-hairline) solid var(--line);
  }

  .row {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 6);
    min-height: 34px;
  }

  .what {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    min-width: 0;
  }

  .icon {
    display: inline-flex;
    color: var(--muted);
  }

  h1 {
    margin: 0;
    overflow: hidden;
    font-family: var(--type-pageTitle-font);
    font-size: var(--type-pageTitle-size);
    font-weight: var(--type-pageTitle-weight);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  h1.code {
    font-family: var(--type-identifier-font);
    font-size: 18px;
    font-weight: 500;
  }

  .count {
    padding: 1px calc(var(--unit) * 4);
    border-radius: var(--radius-round);
    background: var(--sunken);
    color: var(--muted);
    font-size: 13px;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }

  .subtitle {
    color: var(--muted);
    font-size: var(--type-body-size);
  }

  .actions {
    display: flex;
    flex: none;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-left: auto;
  }

  .tabs {
    display: flex;
    gap: calc(var(--unit) * 2);
    margin-top: calc(var(--unit) * 6);
  }

  .tab {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    height: 40px;
    margin-bottom: -1px;
    padding: 0 calc(var(--unit) * 5);
    border: none;
    border-bottom: 2px solid transparent;
    background: none;
    color: var(--muted);
    font-family: var(--type-navigation-font);
    font-size: var(--type-navigation-size);
    font-weight: var(--type-navigation-weight);
    cursor: pointer;
  }

  .tab:hover {
    color: var(--text);
    text-decoration: none;
  }

  /* The tab chosen is marked by its colour and its underline alone, never by a heavier weight, which
     would widen it and move every tab after it. */
  .tab[aria-current="page"],
  .tab[aria-pressed="true"] {
    border-bottom-color: var(--accent);
    color: var(--text);
  }

  .badge {
    padding: 0 calc(var(--unit) * 3);
    border-radius: var(--radius-round);
    background: var(--sunken);
    color: var(--muted);
    font-size: 12px;
    font-variant-numeric: tabular-nums;
  }
</style>
