<script lang="ts">
  import { follow, type Place } from "../lib/place.svelte";
  import type { Route } from "../lib/route";
  import { trailOf } from "../lib/trail";
  import Icon from "./Icon.svelte";

  // The sheet's top row: where the screen is, as a trail of links back up to the home, and the search
  // field that opens the palette as : does, at the top of every screen since the mouse comes first.
  // The caller and their notifications are in the sidebar.
  let {
    route,
    place,
    onmenu,
    onsearch,
  }: {
    route: Route;
    place: Place;
    onmenu?: () => void;
    onsearch?: () => void;
  } = $props();

  // The trail: each step a link but the last, which is where the screen is.
  const trail = $derived(trailOf(route));

</script>

<!-- The home's trail would be its own name and nothing above it, so the home has none; the bar is still
     drawn, holding the search field and, on a phone, the way to the navigation. -->
{#if route.kind !== "landing" || onmenu || onsearch}
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
  {#if onsearch}
    <button class="search" aria-label="Search" onclick={onsearch}><Icon name="control-search" /><span class="label">Search</span><kbd>:</kbd></button>
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

  /* The bar is the sheet's first row, drawn up into the sheet's own top padding. */
  .bar {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    height: var(--bar-keyLine);
    margin: calc(-1 * (var(--padding-page) + 6px)) 0 calc(var(--unit) * 4);
  }

  .search {
    display: inline-flex;
    flex: none;
    align-items: center;
    gap: calc(var(--unit) * 4);
    width: 240px;
    height: var(--control-height);
    margin-left: auto;
    padding: 0 calc(var(--unit) * 3) 0 calc(var(--unit) * 5);
    border: none;
    border-radius: var(--radius-control);
    background: var(--bg);
    color: var(--faint);
    font-family: var(--type-control-font);
    font-size: var(--type-control-size);
    cursor: pointer;
    transition: color var(--motion-dur1) var(--motion-easeOut);
  }

  .search:hover {
    color: var(--muted);
  }

  .search kbd {
    min-width: 18px;
    margin-left: auto;
    padding: 0 calc(var(--unit) * 2);
    border-radius: var(--radius-chip);
    background: var(--raised);
    box-shadow: 0 0 0 var(--border-hairline) var(--lineStrong);
    color: var(--muted);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
    line-height: 18px;
    text-align: center;
  }

  /* On a phone the bar runs from one edge of the window to the other and stays at the top as the
     screen scrolls, since it holds the way to the navigation; the trail keeps where the screen is and
     drops the way back up, which the drawer gives. */
  @media (max-width: 759px) {
    .bar {
      position: sticky;
      top: var(--elsewhere, 0px);
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

    /* A phone has no : to press and no room for the word: the field is its icon. */
    .search {
      width: 36px;
      padding: 0;
      justify-content: center;
      background: none;
    }

    .search .label,
    .search kbd {
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
