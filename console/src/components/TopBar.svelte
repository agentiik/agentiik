<script lang="ts">
  import type { Me, Namespace } from "../api/client";
  import { follow, type Place } from "../lib/place.svelte";
  import type { View } from "../lib/route";
  import { said } from "../lib/notifications";
  import { apply, chosen, type Ground } from "../lib/theme";
  import Icon from "./Icon.svelte";
  import NamespaceSwitcher from "./NamespaceSwitcher.svelte";
  import Popover from "./Popover.svelte";

  // The top bar: the mark, the namespace switcher, the views the caller may open in that namespace,
  // then whether the installation answers, the caller's notifications and the caller. There is no
  // sidebar, so that the panes below take the window's width, as in agk console.
  let {
    me,
    namespaces,
    namespace,
    view,
    shown,
    answering,
    place,
    onsignout,
    ondismiss,
  }: {
    me: Me;
    namespaces: Namespace[];
    namespace: string | undefined;
    view: View | undefined;
    shown: { view: View; label: string }[];
    answering: boolean;
    place: Place;
    onsignout: () => void;
    ondismiss: (id: string) => void;
  } = $props();

  let ground = $state<Ground>(chosen(globalThis.localStorage));

  function choose(g: Ground) {
    ground = g;
    apply(document.documentElement, globalThis.localStorage, g);
  }

  const initial = $derived((me.user?.display_name ?? me.principal).trim().charAt(0).toUpperCase());
  const home = { kind: "landing" as const };
</script>

<header class="bar">
  <a class="brand" href={place.href(home)} onclick={follow(place, home)} aria-label="Agentiik, your home">
    <svg viewBox="0 0 16 14" aria-hidden="true"
      ><rect x="0" y="0" width="16" height="4" rx="1" /><g opacity="0.62"><rect x="0" y="6" width="7" height="4" rx="1" /><rect x="9" y="6" width="7" height="4" rx="1" /></g><g
        opacity="0.3"><rect x="0" y="12" width="16" height="2" rx="1" /></g
      ></svg>
    <span class="wordmark">agentiik</span>
  </a>

  {#if namespaces.length > 0 || namespace}
    <NamespaceSwitcher {namespaces} principal={me.principal} current={namespace} view={view ?? "runs"} {place} every={place.route.kind === "landing"} />
  {/if}

  {#if namespace}
    <nav class="views" aria-label="Views">
      {#each shown as item (item.view)}
        {@const route = { kind: "namespace" as const, namespace, view: item.view }}
        <a class="view" class:open={item.view === view} aria-current={item.view === view ? "page" : undefined} href={place.href(route)} onclick={follow(place, route)}>{item.label}</a>
      {/each}
    </nav>
  {/if}

  <div class="end">
    <span class="live" class:lost={!answering} role="status">
      <span class="dot" aria-hidden="true"></span>{answering ? "live" : "not answering"}
    </span>

    <Popover label={me.notifications.length === 0 ? "Notifications, none" : `Notifications, ${me.notifications.length}`} align="end" width={340}>
      {#snippet button()}
        <span class="bell">
          <Icon name="control-notifications" />
          {#if me.notifications.length > 0}<span class="count">{me.notifications.length}</span>{/if}
        </span>
      {/snippet}
      {#snippet children()}
        {#if me.notifications.length === 0}
          <p class="empty">Nothing to tell you.</p>
        {:else}
          <ul class="notifications">
            {#each me.notifications as notice (notice.id)}
              <li>
                <span class="said">{said(notice)}</span>
                <time class="faint mono" datetime={notice.at}>{notice.at}</time>
                <button class="control" onclick={() => ondismiss(notice.id)}>Dismiss</button>
              </li>
            {/each}
          </ul>
        {/if}
      {/snippet}
    </Popover>

    <Popover label="You, {me.principal}" align="end" width={240}>
      {#snippet button()}
        <span class="who">
          <span class="avatar" aria-hidden="true">{initial}</span>
          <span class="login">{me.principal}</span>
          <Icon name="control-expand" size={14} />
        </span>
      {/snippet}
      {#snippet children(close)}
        <div class="menu">
          {#if me.user}<p class="name">{me.user.display_name}</p>{/if}
          <a class="entry" href={place.href({ kind: "account" })} onclick={(e) => { follow(place, { kind: "account" })(e); close(); }}>Your account</a>
          {#if me.admin}
            <a class="entry" href={place.href({ kind: "users" })} onclick={(e) => { follow(place, { kind: "users" })(e); close(); }}>Users, groups and namespaces</a>
            <a class="entry" href={place.href({ kind: "runners" })} onclick={(e) => { follow(place, { kind: "runners" })(e); close(); }}>Runners and pools</a>
          {/if}
          <fieldset class="ground">
            <legend>Ground</legend>
            {#each [["system", "System"], ["light", "Light"], ["dark", "Dark"]] as [value, label] (value)}
              <button class="choice" aria-pressed={ground === value} onclick={() => choose(value as Ground)}>{label}</button>
            {/each}
          </fieldset>
          <button class="entry signout" onclick={onsignout}><Icon name="control-signout" size={14} />Sign out</button>
        </div>
      {/snippet}
    </Popover>
  </div>
</header>

<style>
  .bar {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    height: var(--bar-top);
    padding: 0 var(--padding-page);
    border-bottom: var(--border-hairline) solid var(--line);
    background: var(--surface);
  }

  .brand {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-right: calc(var(--unit) * 4);
    color: var(--text);
  }

  .brand:hover {
    text-decoration: none;
  }

  .brand svg {
    width: 18px;
    height: 16px;
    fill: var(--accent);
  }

  .wordmark {
    font-family: var(--type-wordmark-font);
    font-size: var(--type-wordmark-size);
    font-weight: var(--type-wordmark-weight);
    letter-spacing: var(--type-wordmark-tracking);
    text-transform: var(--type-wordmark-case);
  }

  .views {
    display: flex;
    gap: calc(var(--unit) * 2);
  }

  .view {
    display: inline-flex;
    align-items: center;
    height: 29px;
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid transparent;
    border-radius: var(--radius-control);
    color: var(--muted);
    font-family: var(--type-navigation-font);
    font-size: var(--type-navigation-size);
    font-weight: var(--type-navigation-weightActive);
  }

  .view:hover {
    color: var(--text);
    text-decoration: none;
  }

  .view.open {
    border-color: var(--lineStrong);
    background: var(--raised);
    color: var(--text);
    font-weight: 600;
  }

  .end {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 7);
    margin-left: auto;
  }

  .live {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    color: var(--succeeded);
    font-size: var(--type-control-size);
    font-weight: 500;
  }

  .live .dot {
    width: 7px;
    height: 7px;
    border-radius: var(--radius-round);
    background: currentColor;
  }

  .live.lost {
    color: var(--failed);
  }

  .bell {
    position: relative;
    display: inline-flex;
    color: var(--muted);
  }

  .count {
    position: absolute;
    top: -7px;
    right: -8px;
    min-width: 14px;
    height: 14px;
    padding: 0 3px;
    border-radius: var(--radius-round);
    background: var(--waiting);
    color: var(--bg);
    font-size: 10px;
    font-weight: 700;
    line-height: 14px;
    text-align: center;
  }

  .who {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    color: var(--muted);
  }

  .avatar {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    border: var(--border-hairline) solid var(--accentLine);
    border-radius: var(--radius-round);
    background: var(--accentDim);
    color: var(--accent);
    font-size: 11.5px;
    font-weight: 600;
  }

  .login {
    color: var(--text);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-max);
    font-weight: 600;
  }

  .menu {
    display: flex;
    flex-direction: column;
  }

  .name {
    margin: calc(var(--unit) * 2) calc(var(--unit) * 4) calc(var(--unit) * 3);
    color: var(--muted);
  }

  .entry {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: none;
    border-radius: var(--radius-control);
    background: none;
    color: var(--text);
    font-size: var(--type-control-size);
    text-align: left;
    cursor: pointer;
  }

  .entry:hover {
    background: var(--surface);
    text-decoration: none;
  }

  .ground {
    display: flex;
    gap: calc(var(--unit) * 2);
    margin: calc(var(--unit) * 3) calc(var(--unit) * 4);
    padding: 0;
    border: none;
  }

  .ground legend {
    float: left;
    margin-right: auto;
    color: var(--muted);
    font-size: var(--type-control-size);
    line-height: 24px;
  }

  .choice {
    height: 24px;
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-chip);
    background: var(--surface);
    font-size: 11.5px;
    cursor: pointer;
  }

  .choice[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
    font-weight: 600;
  }

  .signout {
    margin-top: calc(var(--unit) * 2);
    border-top: var(--border-hairline) solid var(--line);
    border-radius: 0;
    padding-top: calc(var(--unit) * 4);
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
    line-height: 1.45;
  }

  .notifications button {
    grid-row: span 2;
    align-self: center;
  }
</style>
