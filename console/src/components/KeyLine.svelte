<script lang="ts">
  // The key line, at the foot of the window: the keys of the view, each named by its effect, as agk
  // console's bottom line names them. They are shortcuts beside the buttons, never the only way.
  export type Key = { keys: string[]; effect: string };

  let { keys, version }: { keys: Key[]; version: string } = $props();
</script>

<footer class="line">
  <ul aria-label="Keys of this view">
    {#each keys as key (key.effect)}
      <li>
        {#each key.keys as k (k)}<kbd>{k}</kbd>{/each}
        <span>{key.effect}</span>
      </li>
    {/each}
  </ul>
  <span class="version">agentiik {version}</span>
</footer>

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
  }

  li {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .version {
    margin-left: auto;
    color: var(--faint);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
  }
</style>
