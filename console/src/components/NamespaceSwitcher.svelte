<script lang="ts">
  import type { Namespace } from "../api/client";
  import { ordered } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import type { View } from "../lib/route";
  import Icon from "./Icon.svelte";
  import Popover from "./Popover.svelte";

  // The namespace switcher, at the head of the sidebar's views: the caller's personal namespace first,
  // the shared ones grouped after it, and for an administrator the other users' personal ones last.
  // Choosing one opens the same view in it, since a person switching namespace is usually comparing the
  // same thing. Folded, it is the namespace's initial alone.
  let {
    namespaces,
    principal,
    current,
    view,
    place,
    folded = false,
  }: { namespaces: Namespace[]; principal: string; current: string | undefined; view: View; place: Place; folded?: boolean } = $props();

  const groups = $derived(ordered(namespaces, principal));
  const record = $derived(namespaces.find((n) => n.name === current));
</script>

<Popover label="Switch namespace" block width={248}>
  {#snippet button()}
    <span class="current" class:folded title={folded ? current : undefined}>
      <span class="initial" aria-hidden="true">{(current ?? "?").charAt(0).toUpperCase()}</span>
      {#if !folded}
        <span class="what">
          <span class="name">{current ?? "No namespace"}</span>
          <span class="kind">{record ? (record.kind === "personal" ? (record.name === principal ? "Your namespace" : "Personal") : "Shared") : "Namespace"}</span>
        </span>
        <Icon name="control-expand" size={14} />
      {/if}
    </span>
  {/snippet}
  {#snippet children(close)}
    <nav aria-label="Namespaces">
      {#if groups.own}
        <p class="heading">Yours</p>
        {@render item(groups.own, close)}
      {/if}
      {#if groups.shared.length > 0}
        <p class="heading">Shared</p>
        {#each groups.shared as namespace (namespace.name)}
          {@render item(namespace, close)}
        {/each}
      {/if}
      {#if groups.personal.length > 0}
        <p class="heading">Other people's</p>
        {#each groups.personal as namespace (namespace.name)}
          {@render item(namespace, close)}
        {/each}
      {/if}
      {#if namespaces.length === 0}
        <p class="none">No namespace holds a grant of yours.</p>
      {/if}
    </nav>
  {/snippet}
</Popover>

{#snippet item(namespace: Namespace, close: () => void)}
  {@const route = { kind: "namespace" as const, namespace: namespace.name, view }}
  <a
    class="item"
    class:chosen={namespace.name === current}
    aria-current={namespace.name === current ? "page" : undefined}
    href={place.href(route)}
    onclick={(event) => {
      follow(place, route)(event);
      close();
    }}
  >
    <span>{namespace.name}</span>
    {#if namespace.owner && namespace.kind === "shared"}<span class="owner">{namespace.owner}</span>{/if}
  </a>
{/snippet}

<style>
  .current {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    width: 100%;
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--muted);
    text-align: left;
  }

  .current:hover {
    border-color: var(--lineStrong);
  }

  .current.folded {
    justify-content: center;
    padding: calc(var(--unit) * 3) 0;
  }

  .initial {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border-radius: var(--radius-control);
    background: var(--accentDim);
    color: var(--accent);
    font-size: 13px;
    font-weight: 700;
  }

  .what {
    display: flex;
    flex: 1;
    flex-direction: column;
    min-width: 0;
    --leading: 1.25;
  }

  .name {
    overflow: hidden;
    color: var(--text);
    font-size: var(--type-name-size);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .kind {
    color: var(--faint);
    font-size: 12px;
  }

  .heading {
    margin: calc(var(--unit) * 3) calc(var(--unit) * 4) calc(var(--unit) * 2);
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .heading:first-child {
    margin-top: calc(var(--unit) * 2);
  }

  .item {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: calc(var(--unit) * 4);
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    color: var(--text);
    font-size: var(--type-name-size);
  }

  .item:hover {
    background: var(--surface);
    text-decoration: none;
  }

  .item.chosen {
    background: var(--accentDim);
    color: var(--accent);
    font-weight: 600;
  }

  .owner {
    color: var(--faint);
    font-size: 12px;
    font-weight: 400;
  }

  .none {
    margin: calc(var(--unit) * 4);
    color: var(--muted);
  }
</style>
