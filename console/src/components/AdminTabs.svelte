<script lang="ts">
  import { follow, type Place } from "../lib/place.svelte";

  // What an administrator manages of the installation, one tab each: the users, the groups they are
  // put in, and the namespaces with their owners and quotas.
  let { place, current }: { place: Place; current: "users" | "groups" | "namespaces" } = $props();

  const tabs = [
    { kind: "users" as const, label: "Users" },
    { kind: "groups" as const, label: "Groups" },
    { kind: "namespaces" as const, label: "Namespaces" },
  ];
</script>

<nav class="sub" aria-label="What an administrator manages">
  <span class="mono where">installation / Administration</span>
  {#each tabs as t (t.kind)}
    {#if t.kind === current}
      <span class="tab" aria-current="page">{t.label}</span>
    {:else}
      <a class="tab" href={place.href({ kind: t.kind })} onclick={follow(place, { kind: t.kind })}>{t.label}</a>
    {/if}
  {/each}
</nav>

<style>
  .sub {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
    margin: calc(var(--unit) * -3) 0 calc(var(--unit) * 7);
  }

  .where {
    margin-right: calc(var(--unit) * 6);
    font-weight: 600;
  }

  .tab {
    display: inline-flex;
    align-items: center;
    height: 29px;
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid transparent;
    border-radius: var(--radius-control);
    color: var(--muted);
    font-size: var(--type-navigation-size);
    font-weight: 500;
    text-decoration: none;
  }

  .tab[aria-current="page"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }
</style>
