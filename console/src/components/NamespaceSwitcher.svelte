<script lang="ts">
  import type { Namespace } from "../api/client";
  import { ordered } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import type { View } from "../lib/route";
  import { pictureOf } from "../lib/namespaces";
  import Icon from "./Icon.svelte";
  import NamespaceMark from "./NamespaceMark.svelte";
  import Popover from "./Popover.svelte";

  // The namespace switcher, at the head of the sidebar's views: the caller's personal namespace first,
  // the shared ones grouped after it, and for an administrator the other users' personal ones last.
  // Choosing one opens the same view in it, since a person switching namespace is usually comparing the
  // same thing. Folded, it is the namespace's picture or initial alone. Its foot creates a namespace,
  // where the caller is a person, who may.
  let {
    namespaces,
    principal,
    current,
    view,
    place,
    folded = false,
    oncreate,
  }: { namespaces: Namespace[]; principal: string; current: string | undefined; view: View; place: Place; folded?: boolean; oncreate?: () => void } = $props();

  const groups = $derived(ordered(namespaces, principal));
  const record = $derived(namespaces.find((n) => n.name === current));
</script>

<Popover label="Switch namespace" block width={248}>
  {#snippet button()}
    <span class="current" class:folded title={folded ? current : undefined}>
      <NamespaceMark name={current ?? "?"} src={pictureOf(record)} />
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
    {#if oncreate}
      <div class="foot">
        <button
          class="item create"
          onclick={() => {
            close();
            oncreate();
          }}><Icon name="control-add" size={14} />New namespace</button
        >
      </div>
    {/if}
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
    <span class="named"><NamespaceMark name={namespace.name} src={pictureOf(namespace)} size={20} /><span>{namespace.name}</span></span>
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
    align-items: center;
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

  .named {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    min-width: 0;
  }

  .named > span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .foot {
    margin-top: calc(var(--unit) * 2);
    padding-top: calc(var(--unit) * 2);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
  }

  .create {
    justify-content: flex-start;
    gap: calc(var(--unit) * 3);
    width: 100%;
    border: none;
    background: none;
    color: var(--muted);
    text-align: left;
  }

  .create:hover {
    color: var(--text);
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
