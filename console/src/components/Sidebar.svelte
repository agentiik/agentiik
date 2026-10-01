<script lang="ts">
  import type { Me, Namespace } from "../api/client";
  import { follow, type Place } from "../lib/place.svelte";
  import type { Route, View } from "../lib/route";
  import AccountMenu from "./AccountMenu.svelte";
  import Icon from "./Icon.svelte";
  import NamespaceSwitcher from "./NamespaceSwitcher.svelte";
  import Notifications from "./Notifications.svelte";

  // The navigation, down the left of every screen: the home and the caller's notifications, the views
  // of one namespace, what an administrator manages, and the caller's account. Its entries never move: the namespace's views are
  // listed in one order, those of the namespace last opened while the home or the account is shown, so
  // that a menu is where it was whichever screen is open. Folded, it keeps its icons alone; in a
  // window too narrow to choose otherwise it is folded or a drawer whatever was chosen, and offers no
  // fold.
  let {
    me,
    namespaces,
    namespace,
    shown,
    route,
    place,
    folded,
    foldable = true,
    onfold,
    version,
    onsignout,
    ondismiss,
  }: {
    me: Me;
    namespaces: Namespace[];
    namespace: string | undefined;
    shown: { view: View; label: string }[];
    route: Route;
    place: Place;
    folded: boolean;
    foldable?: boolean;
    onfold: (value: boolean) => void;
    version: string;
    onsignout: () => void;
    ondismiss: (id: string) => void;
  } = $props();

  const icon: Record<View, string> = { workflows: "control-workflows", statistics: "control-statistics", sharing: "control-share", settings: "control-settings" };
  const home = { kind: "landing" as const };
  const admin = [
    { kind: "runners" as const, label: "Runners", icon: "control-runners" },
    { kind: "users" as const, label: "Users", icon: "control-users" },
    { kind: "groups" as const, label: "Groups", icon: "control-groups" },
    { kind: "namespaces" as const, label: "Namespaces", icon: "control-namespaces" },
  ];
  const current = $derived(route.kind === "namespace" && route.namespace === namespace ? route.view : undefined);
</script>

<nav class="sidebar" class:folded aria-label="Navigation">
  <a class="brand" href={place.href(home)} onclick={follow(place, home)} aria-label="Agentiik, your home">
    <svg viewBox="0 0 16 14" aria-hidden="true"
      ><rect x="0" y="0" width="16" height="4" rx="1" /><g opacity="0.62"><rect x="0" y="6" width="7" height="4" rx="1" /><rect x="9" y="6" width="7" height="4" rx="1" /></g><g
        opacity="0.3"><rect x="0" y="12" width="16" height="2" rx="1" /></g
      ></svg>
    {#if !folded}<span class="wordmark">agentiik</span>{/if}
  </a>

  <ul class="entries">
    <li>
      <a class="entry" class:open={route.kind === "landing"} aria-current={route.kind === "landing" ? "page" : undefined} href={place.href(home)} onclick={follow(place, home)} title={folded ? "Home" : undefined}>
        <Icon name="control-home" /><span class="label">Home</span>
      </a>
    </li>
    <li><Notifications {me} {folded} {ondismiss} /></li>
  </ul>

  <div class="switcher">
    <NamespaceSwitcher {namespaces} principal={me.principal} current={namespace} view={current ?? "workflows"} {place} {folded} />
  </div>

  {#if namespace}
    <ul class="entries" aria-label="Views of {namespace}">
      {#each shown as item (item.view)}
        {@const to = { kind: "namespace" as const, namespace, view: item.view }}
        <li>
          <a class="entry" class:open={current === item.view} aria-current={current === item.view ? "page" : undefined} href={place.href(to)} onclick={follow(place, to)} title={folded ? item.label : undefined}>
            <Icon name={icon[item.view]} /><span class="label">{item.label}</span>
          </a>
        </li>
      {/each}
    </ul>
  {/if}

  {#if me.admin}
    {#if !folded}<p class="section">Installation</p>{:else}<hr />{/if}
    <ul class="entries" aria-label="Installation">
      {#each admin as a (a.kind)}
        <li>
          <a class="entry" class:open={route.kind === a.kind} aria-current={route.kind === a.kind ? "page" : undefined} href={place.href({ kind: a.kind })} onclick={follow(place, { kind: a.kind })} title={folded ? a.label : undefined}>
            <Icon name={a.icon} /><span class="label">{a.label}</span>
          </a>
        </li>
      {/each}
    </ul>
  {/if}

  <ul class="entries foot">
    <li class="account" class:open={route.kind === "account"}><AccountMenu {me} {place} {version} {folded} {onsignout} /></li>
    {#if foldable}
      <li>
        <button class="entry" aria-pressed={folded} onclick={() => onfold(!folded)} title={folded ? "Unfold the sidebar" : undefined}>
          <Icon name="control-sidebar" /><span class="label">Fold the sidebar</span>
        </button>
      </li>
    {/if}
  </ul>
</nav>

<style>
  .sidebar {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 2);
    width: var(--sidebar-width);
    height: 100%;
    padding: 0 calc(var(--unit) * 5) calc(var(--unit) * 5);
    overflow-y: auto;
    border-right: var(--border-hairline) solid var(--line);
    background: var(--surface);
  }

  .sidebar.folded {
    width: var(--sidebar-collapsed);
    padding-right: calc(var(--unit) * 4);
    padding-left: calc(var(--unit) * 4);
  }

  .brand {
    display: flex;
    flex: none;
    align-items: center;
    gap: calc(var(--unit) * 5);
    height: var(--bar-top);
    padding: 0 calc(var(--unit) * 4);
    color: var(--text);
  }

  .folded .brand {
    justify-content: center;
    padding: 0;
  }

  .brand:hover {
    text-decoration: none;
  }

  .brand svg {
    width: 20px;
    height: 18px;
    fill: var(--accent);
  }

  .wordmark {
    font-family: var(--type-wordmark-font);
    font-size: var(--type-wordmark-size);
    font-weight: var(--type-wordmark-weight);
    letter-spacing: var(--type-wordmark-tracking);
    text-transform: var(--type-wordmark-case);
  }

  .switcher {
    margin: calc(var(--unit) * 4) 0 calc(var(--unit) * 1);
  }

  .entries {
    display: flex;
    flex-direction: column;
    gap: 1px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .entry {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    width: 100%;
    height: 34px;
    padding: 0 calc(var(--unit) * 4);
    border: none;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    font-family: var(--type-navigation-font);
    font-size: var(--type-navigation-size);
    font-weight: var(--type-navigation-weight);
    text-align: left;
    cursor: pointer;
  }

  .folded .entry {
    justify-content: center;
    padding: 0;
  }

  .folded .label {
    display: none;
  }

  .entry:hover {
    background: var(--raised);
    color: var(--text);
    text-decoration: none;
  }

  .entry.open {
    background: var(--accentDim);
    color: var(--accent);
    font-weight: var(--type-navigation-weightActive);
  }

  .section {
    margin: calc(var(--unit) * 7) calc(var(--unit) * 4) calc(var(--unit) * 2);
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
  }

  hr {
    width: 100%;
    margin: calc(var(--unit) * 4) 0;
    border: none;
    border-top: var(--border-hairline) solid var(--line);
  }

  .foot {
    margin-top: auto;
    padding-top: calc(var(--unit) * 4);
    border-top: var(--border-hairline) solid var(--line);
  }
</style>
