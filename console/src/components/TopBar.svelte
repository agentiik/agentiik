<script lang="ts">
  import type { Me } from "../api/client";
  import { follow, type Place } from "../lib/place.svelte";
  import type { Route } from "../lib/route";
  import { said } from "../lib/notifications";
  import Icon from "./Icon.svelte";
  import Popover from "./Popover.svelte";

  // The line above the screen's own head: where the screen is, as a trail of links back up to the
  // home, then whether the installation answers and the caller's notifications. It is the screen's
  // first line rather than a bar of its own, so that it takes no room the screen could use; the caller
  // is at the foot of the sidebar.
  let {
    me,
    route,
    answering,
    live,
    place,
    ondismiss,
    onmenu,
  }: {
    me: Me;
    route: Route;
    answering: boolean;
    live: boolean;
    place: Place;
    ondismiss: (id: string) => void;
    onmenu?: () => void;
  } = $props();


  // Whether what the screen shows is current, in a word: both connections are retried on their own.
  const standing = $derived(!answering ? "unreachable" : live ? "live" : "reconnecting");

  const labels: Record<string, string> = { runs: "Runs", workflows: "Workflows", statistics: "Statistics", sharing: "Sharing", settings: "Settings" };
  const tabs: Record<string, string> = { files: "Files", statistics: "Statistics", mcp: "MCP", graph: "Graph", profile: "Profile", credentials: "Sign-in methods", tokens: "API tokens", "service-accounts": "Service accounts" };

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
    <span class="live" class:lost={!answering} class:waiting={answering && !live} role="status">
      <span class="dot" aria-hidden="true"></span><span class="word">{standing}</span>
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

  .bar {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    height: 32px;
    margin: 0 0 calc(var(--unit) * 6);
  }

  /* On a phone the bar runs from one edge of the window to the other and stays at the top as the
     screen scrolls, since it holds the way to the navigation; the trail keeps where the screen is and
     drops the way back up, which the drawer gives, and the installation's state keeps its dot. */
  @media (max-width: 759px) {
    .bar {
      position: sticky;
      top: -16px;
      z-index: 30;
      height: 48px;
      margin: -16px -16px 16px;
      padding: 0 16px;
      box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
      background: var(--surface);
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
  }


  .trail {
    min-width: 0;
  }

  .trail ol {
    display: flex;
    align-items: baseline;
    height: 1lh;
    margin: 0;
    padding: 0;
    overflow: hidden;
    list-style: none;
    white-space: nowrap;
  }

  /* A separator and a name read along one line, whatever face the name is set in, and the line keeps
     its own height, one line of the bar's text, where a name in code and one in Archivo aligned on
     their baselines would together be a pixel taller and centre half a pixel down. */
  .trail li {
    display: inline-flex;
    align-items: baseline;
    height: 1lh;
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
    width: 8px;
    height: 8px;
    border-radius: var(--radius-round);
    background: currentColor;
  }

  .live.lost {
    color: var(--failed);
  }

  /* Answering, and not telling what changed: what is shown is read again when the connection
     opens, and until then stays as it was read. */
  .live.waiting {
    color: var(--muted);
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

  .notifications button {
    grid-row: span 2;
    align-self: center;
  }
</style>
