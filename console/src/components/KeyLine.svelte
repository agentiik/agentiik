<script lang="ts">
  import { shown, type Keys } from "../lib/keys.svelte";
  import Icon from "./Icon.svelte";

  // The key line, at the foot of the window: the keys of the view, each named by its effect, as agk
  // console's bottom line names them. They are shortcuts beside the buttons, never the only way, and
  // ? lists every one of them, those the line has no room for among them.
  let { keys, version }: { keys: Keys; version: string } = $props();

  const all = $derived(keys.bindings);
  const line = $derived(all.filter((b) => b.inLine !== false));
</script>

<footer class="line">
  <ul aria-label="Keys of this view">
    {#each line as b (b.effect)}
      <li>
        {#each b.brief ?? b.keys as k (k)}<kbd>{shown(k)}</kbd>{/each}
        <span>{b.effect}</span>
      </li>
    {/each}
  </ul>
  <span class="version">agentiik {version}</span>
</footer>

{#if keys.listing}
  <div class="listing" role="dialog" aria-label="Every key of this view">
    <header>
      <strong>Keys</strong>
      <button class="close" aria-label="Close the list of keys" onclick={() => (keys.listing = false)}><Icon name="control-close" size={12} /></button>
    </header>
    <table>
      <tbody>
        {#each all as b (b.effect)}
          <tr>
            <td class="keys">{#each b.keys as k, i (k)}{#if i > 0}{" "}{/if}<kbd>{shown(k)}</kbd>{/each}</td>
            <td>{b.effect}</td>
          </tr>
        {/each}
      </tbody>
    </table>
    <p class="faint">A key does nothing while a field is typed into. Every one stands beside a button or a link that does the same.</p>
  </div>
{/if}

<style>
  .line {
    display: flex;
    align-items: center;
    height: var(--bar-keyLine);
    padding: 0 var(--padding-page);
    border-top: var(--border-hairline) solid var(--line);
    background: var(--surface);
  }

  ul {
    display: flex;
    gap: calc(var(--unit) * 9);
    margin: 0;
    padding: 0;
    list-style: none;
    overflow: hidden;
    white-space: nowrap;
  }

  li {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  kbd {
    min-width: 18px;
    padding: 0 calc(var(--unit) * 2);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-chip);
    background: var(--raised);
    color: var(--text);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
    line-height: 18px;
    text-align: center;
  }

  .version {
    margin-left: auto;
    padding-left: calc(var(--unit) * 9);
    color: var(--faint);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
    white-space: nowrap;
  }

  .listing {
    position: fixed;
    right: var(--padding-page);
    bottom: calc(var(--bar-keyLine) + var(--padding-page));
    z-index: 20;
    width: 380px;
    padding: var(--padding-panel);
    border: var(--border-hairline) solid var(--accent);
    border-radius: var(--radius-pane);
    background: var(--surface);
    box-shadow: 0 8px 24px rgb(0 0 0 / 12%);
  }

  .listing header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  .close {
    display: inline-flex;
    padding: calc(var(--unit) * 2);
    border: 0;
    background: none;
    color: var(--muted);
    cursor: pointer;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  td {
    padding: calc(var(--unit) * 2) 0;
    vertical-align: baseline;
  }

  .keys {
    width: 40%;
    white-space: nowrap;
  }

  .faint {
    margin: calc(var(--unit) * 5) 0 0;
    font-size: var(--type-control-size);
  }
</style>
