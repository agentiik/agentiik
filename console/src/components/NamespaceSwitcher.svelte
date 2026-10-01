<script lang="ts">
  import type { Namespace } from "../api/client";
  import { ordered } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import type { View } from "../lib/route";
  import Icon from "./Icon.svelte";
  import Popover from "./Popover.svelte";

  // The namespace switcher, in the top bar: the caller's personal namespace first, the shared ones
  // grouped after it, and for an administrator the other users' personal ones last. Choosing one opens
  // the same view in it, since a person switching namespace is usually comparing the same thing.
  let {
    namespaces,
    principal,
    current,
    view,
    place,
  }: { namespaces: Namespace[]; principal: string; current: string | undefined; view: View; place: Place } = $props();

  const groups = $derived(ordered(namespaces, principal));
</script>

<Popover label="Switch namespace">
  {#snippet button()}
    <span class="current">
      <span class="name">{current ?? "no namespace"}</span>
      <Icon name="control-expand" size={14} />
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
    <span class="mono">{namespace.name}</span>
    {#if namespace.owner && namespace.kind === "shared"}<span class="owner">{namespace.owner}</span>{/if}
  </a>
{/snippet}

<style>
  .current {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    height: 29px;
    padding: 0 calc(var(--unit) * 4) 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--accentLine);
    border-radius: var(--radius-control);
    background: var(--accentDim);
    color: var(--accent);
  }

  .name {
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-max);
    font-weight: 600;
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
    font-size: var(--type-identifier-size-max);
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
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
    font-weight: 400;
  }

  .none {
    margin: calc(var(--unit) * 4);
    color: var(--muted);
  }
</style>
