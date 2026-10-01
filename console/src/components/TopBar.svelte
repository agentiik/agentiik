<script lang="ts">
  import type { Me } from "../api/client";
  import { follow, type Place } from "../lib/place.svelte";
  import type { Route } from "../lib/route";
  import { said } from "../lib/notifications";
  import { apply, chosen, type Ground } from "../lib/theme";
  import Avatar from "./Avatar.svelte";
  import Icon from "./Icon.svelte";
  import Popover from "./Popover.svelte";

  // The bar above the screen: where the screen is, as a trail of links back up to the home, then
  // whether the installation answers, the caller's notifications and the caller. The views themselves
  // are the sidebar's, so that nothing here moves with the namespace or the screen.
  let {
    me,
    route,
    answering,
    place,
    onsignout,
    ondismiss,
    onmenu,
  }: {
    me: Me;
    route: Route;
    answering: boolean;
    place: Place;
    onsignout: () => void;
    ondismiss: (id: string) => void;
    onmenu?: () => void;
  } = $props();

  let ground = $state<Ground>(chosen(globalThis.localStorage));

  function choose(g: Ground) {
    ground = g;
    apply(document.documentElement, globalThis.localStorage, g);
  }

  const labels: Record<string, string> = { runs: "Runs", workflows: "Workflows", statistics: "Statistics", sharing: "Sharing", settings: "Settings" };
  const tabs: Record<string, string> = { files: "Files", statistics: "Statistics", mcp: "MCP", graph: "Graph", credentials: "Sign-in methods", tokens: "API tokens", "service-accounts": "Service accounts" };

  // The trail: each step a link but the last, which is where the screen is.
  const trail = $derived.by((): { label: string; to?: Route; code?: boolean }[] => {
    const r = route;
    switch (r.kind) {
      case "landing":
        return [{ label: "Home" }];
      case "account":
        return [{ label: "Your account", to: r.tab ? { kind: "account" } : undefined }, ...(r.tab ? [{ label: tabs[r.tab] ?? r.tab }] : [])];
      case "runners":
        return [{ label: "Runners", to: r.tab ? { kind: "runners" } : undefined }, ...(r.tab ? [{ label: "Statistics" }] : [])];
      case "users":
        return [{ label: "Users" }];
      case "groups":
        return [{ label: "Groups" }];
      case "namespaces":
        return [{ label: "Namespaces" }];
      case "unknown":
        return [{ label: "Nothing here" }];
      case "namespace": {
        const out: { label: string; to?: Route; code?: boolean }[] = [{ label: r.namespace, to: { kind: "namespace", namespace: r.namespace, view: "runs" } }];
        const deeper = r.workflow !== undefined || r.run !== undefined;
        out.push({ label: labels[r.view] ?? r.view, to: deeper ? { kind: "namespace", namespace: r.namespace, view: r.view } : undefined });
        if (r.workflow) {
          out.push({ label: r.workflow, to: r.tab ? { kind: "namespace", namespace: r.namespace, view: "workflows", workflow: r.workflow } : undefined });
          if (r.tab) out.push({ label: tabs[r.tab] ?? r.tab });
        }
        if (r.run) {
          out.push({ label: r.run, code: true, to: r.against ? { kind: "namespace", namespace: r.namespace, view: "runs", run: r.run } : undefined });
          if (r.against) out.push({ label: `against ${r.against}`, code: true });
        }
        return out;
      }
    }
  });

  const name = $derived(me.user?.display_name ?? me.principal);
</script>

<header class="bar">
  {#if onmenu}
    <button class="menu-button" aria-label="Open the navigation" onclick={onmenu}><Icon name="control-sidebar" size={18} /></button>
  {/if}
  <nav class="trail" aria-label="Where you are">
    <ol>
      {#each trail as step, i (i)}
        <li>
          {#if i > 0}<span class="sep" aria-hidden="true">/</span>{/if}
          {#if step.to}
            <a class:code={step.code} href={place.href(step.to)} onclick={follow(place, step.to)}>{step.label}</a>
          {:else}
            <span class="here" class:code={step.code} aria-current="page">{step.label}</span>
          {/if}
        </li>
      {/each}
    </ol>
  </nav>

  <div class="end">
    <span class="live" class:lost={!answering} role="status">
      <span class="dot" aria-hidden="true"></span><span class="word">{answering ? "live" : "not answering"}</span>
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
                <time class="faint term" datetime={notice.at}>{notice.at}</time>
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
          <Avatar {name} size={24} />
          <span class="login">{name}</span>
          <Icon name="control-expand" size={14} />
        </span>
      {/snippet}
      {#snippet children(close)}
        <div class="menu">
          <p class="name">{name}<span class="faint">{me.principal}</span></p>
          <a class="entry" href={place.href({ kind: "account" })} onclick={(e) => { follow(place, { kind: "account" })(e); close(); }}>Your account</a>
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
  .menu-button {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    width: 36px;
    height: 36px;
    margin-left: calc(var(--unit) * -3);
    border: none;
    border-radius: var(--radius-control);
    background: none;
    color: var(--text);
    cursor: pointer;
  }

  .menu-button:hover {
    background: var(--raised);
  }

  /* On a phone the trail keeps where the screen is and drops the way back up, which the drawer gives,
     the installation's state keeps its dot, and the caller their face. */
  @media (max-width: 759px) {
    .bar {
      padding: 0 16px;
    }

    .trail li:not(:last-child) {
      display: none;
    }

    .trail li:last-child .sep {
      display: none;
    }

    .live .word {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip-path: inset(50%);
      white-space: nowrap;
    }

    .who .login {
      display: none;
    }
  }

  .bar {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    height: var(--bar-top);
    padding: 0 calc(var(--padding-page) + 6px);
    border-bottom: var(--border-hairline) solid var(--line);
    background: var(--surface);
  }

  .trail {
    min-width: 0;
  }

  .trail ol {
    display: flex;
    align-items: center;
    margin: 0;
    padding: 0;
    overflow: hidden;
    list-style: none;
    white-space: nowrap;
  }

  .trail li {
    display: inline-flex;
    align-items: center;
    min-width: 0;
  }

  .trail a,
  .here {
    overflow: hidden;
    font-size: var(--type-navigation-size);
    text-overflow: ellipsis;
  }

  .trail a {
    color: var(--muted);
  }

  .trail a:hover {
    color: var(--text);
  }

  .here {
    color: var(--text);
    font-weight: 600;
  }

  .sep {
    margin: 0 calc(var(--unit) * 4);
    color: var(--faint);
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


  .login {
    color: var(--text);
    font-size: var(--type-name-size);
    font-weight: 500;
  }

  .menu {
    display: flex;
    flex-direction: column;
  }

  .name {
    display: flex;
    flex-direction: column;
    margin: calc(var(--unit) * 2) calc(var(--unit) * 4) calc(var(--unit) * 3);
    font-weight: 600;
  }

  .name .faint {
    font-size: 12px;
    font-weight: 400;
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
