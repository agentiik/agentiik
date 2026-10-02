<script lang="ts">
  import { follow, type Place } from "../lib/place.svelte";
  import { runAt, runsOf, type Route } from "../lib/route";
  import Icon from "./Icon.svelte";

  // The line above the screen's own head: where the screen is, as a trail of links back up to the
  // home. It is the screen's first line rather than a bar of its own, so that it takes no room the
  // screen could use; the caller and their notifications are in the sidebar.
  let {
    route,
    place,
    onmenu,
  }: {
    route: Route;
    place: Place;
    onmenu?: () => void;
  } = $props();

  const labels: Record<string, string> = { workflows: "Workflows", statistics: "Statistics", sharing: "Sharing", settings: "Settings" };
  const tabs: Record<string, string> = { runs: "Runs", files: "Files", statistics: "Statistics", mcp: "MCP", graph: "Graph", profile: "Profile", credentials: "Sign-in methods", tokens: "API tokens", "service-accounts": "Service accounts" };

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
        const out: { label: string; to?: Route; code?: boolean }[] = [{ label: r.namespace, to: { kind: "namespace", namespace: r.namespace, view: "workflows" } }];
        const deeper = r.workflow !== undefined || r.run !== undefined;
        out.push({ label: labels[r.view] ?? r.view, to: deeper ? { kind: "namespace", namespace: r.namespace, view: r.view } : undefined });
        if (r.workflow) {
          // A run is under its workflow's runs, each a link back up but the last.
          const below = r.tab !== undefined || r.run !== undefined;
          out.push({ label: r.workflow, to: below ? { kind: "namespace", namespace: r.namespace, view: "workflows", workflow: r.workflow } : undefined });
          if (r.run) out.push({ label: "Runs", to: runsOf(r.namespace, r.workflow) });
          else if (r.tab) out.push({ label: tabs[r.tab] ?? r.tab });
        }
        if (r.run) {
          out.push({ label: r.run, code: true, to: r.against ? runAt(r.namespace, r.workflow, r.run) : undefined });
          if (r.against) out.push({ label: `against ${r.against}`, code: true });
        }
        return out;
      }
    }
  });

</script>

<!-- The home's trail would be its own name and nothing above it, so the home has none; on a phone the
     bar is still drawn, holding the way to the navigation. -->
{#if route.kind !== "landing" || onmenu}
<header class="bar">
  {#if onmenu}
    <button class="menu-button" aria-label="Open the navigation" onclick={onmenu}><Icon name="control-sidebar" size={18} /></button>
  {/if}
  {#if route.kind !== "landing"}
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
  {/if}
</header>
{/if}

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
     drops the way back up, which the drawer gives. */
  @media (max-width: 759px) {
    .bar {
      position: sticky;
      top: 0;
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





  /* Answering, and not telling what changed: what is shown is read again when the connection
     opens, and until then stays as it was read. */








</style>
