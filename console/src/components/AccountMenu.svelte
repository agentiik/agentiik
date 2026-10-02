<script lang="ts">
  import type { Me } from "../api/client";
  import { follow, type Place } from "../lib/place.svelte";
  import { photoOf } from "../lib/profile";
  import { apply, chosen, type Ground } from "../lib/theme";
  import Avatar from "./Avatar.svelte";
  import Icon from "./Icon.svelte";
  import Popover from "./Popover.svelte";

  // The caller, at the foot of the sidebar: their face and name, opening on their account, the ground
  // and the version, and signing out.
  let { me, place, version, folded = false, onsignout }: { me: Me; place: Place; version: string; folded?: boolean; onsignout: () => void } = $props();

  let ground = $state<Ground>(chosen(globalThis.localStorage));

  function choose(g: Ground) {
    ground = g;
    apply(document.documentElement, globalThis.localStorage, g);
  }

  const name = $derived(me.user?.display_name ?? me.principal);
</script>

<Popover label="You, {me.principal}" align="start" side="top" width={240} block>
      {#snippet button()}
        <span class="who">
          <Avatar {name} src={photoOf(me)} size={24} />
          {#if !folded}<span class="login">{name}</span>{/if}
        </span>
      {/snippet}
      {#snippet children(close)}
        <div class="menu">
          <p class="name">{name}<span class="faint">{me.principal}</span></p>
          <a class="entry" href={place.href({ kind: "account", tab: "profile" })} onclick={(e) => { follow(place, { kind: "account", tab: "profile" })(e); close(); }}>Your account</a>
          <fieldset class="ground">
            <legend>Ground</legend>
            {#each [["system", "System"], ["light", "Light"], ["dark", "Dark"]] as [value, label] (value)}
              <button class="choice" aria-pressed={ground === value} onclick={() => choose(value as Ground)}>{label}</button>
            {/each}
          </fieldset>
          <button class="entry signout" onclick={onsignout}><Icon name="control-signout" size={14} />Sign out</button>
          <p class="version term">agentiik {version}</p>
        </div>
      {/snippet}
    </Popover>

<style>
  /* Drawn as an entry of the sidebar is, its face where an entry's icon is. */
  .who {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    width: 100%;
    height: 40px;
    padding: 0 calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    color: var(--muted);
  }

  .who:hover {
    background: var(--raised);
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

  .version {
    margin: calc(var(--unit) * 2) calc(var(--unit) * 4) 0;
    color: var(--faint);
    font-size: 12px;
  }
</style>
