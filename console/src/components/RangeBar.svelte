<script lang="ts">
  import type { Snippet } from "svelte";
  import type { Ranged } from "../lib/range.svelte";
  import { described, presets, type Preset } from "../lib/stats";

  // The controls along the top of a statistics page: the range, as a preset or a span a zoom chose,
  // the way back from a zoom, the comparison with the span before, and what the page exports.
  let { ranged, bucket, children }: { ranged: Ranged; bucket: string | undefined; children?: Snippet } = $props();
</script>

<div class="range">
  <div class="presets" role="group" aria-label="Range">
    {#each Object.entries(presets) as [key, p] (key)}
      <button class="preset" aria-pressed={ranged.range.preset === key} onclick={() => ranged.choose(key as Preset)}>{p.label}</button>
    {/each}
  </div>
  <span class="muted mono">{described(ranged.range, bucket)}</span>
  {#if ranged.before.length > 0}<button class="link" onclick={() => ranged.back()}>Back to the range before</button>{/if}
  <label class="compare">
    <input type="checkbox" role="switch" checked={ranged.range.compare} onchange={(e) => ranged.compare(e.currentTarget.checked)} />
    <span class="track" aria-hidden="true"><span class="knob"></span></span>
    Compare with the span before
  </label>
  {#if children}<span class="export">{@render children()}</span>{/if}
</div>

<style>
  .range {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 7);
    margin-bottom: calc(var(--unit) * 10);
    font-size: var(--type-control-size);
  }

  .presets {
    display: flex;
    gap: calc(var(--unit) * 1);
    padding: 2px;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
  }

  .preset {
    height: 25px;
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid transparent;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 500;
    cursor: pointer;
  }

  .preset[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .link {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent);
    font-size: var(--type-control-size);
    cursor: pointer;
  }

  .compare {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    cursor: pointer;
  }

  .compare input {
    position: absolute;
    opacity: 0;
    pointer-events: none;
  }

  .track {
    position: relative;
    width: 29px;
    height: 17px;
    border-radius: var(--radius-round);
    background: var(--lineStrong);
  }

  .knob {
    position: absolute;
    top: 2.5px;
    left: 2.5px;
    width: 12px;
    height: 12px;
    border-radius: var(--radius-round);
    background: var(--raised);
  }

  .compare input:checked + .track {
    background: var(--accent);
  }

  .compare input:checked + .track .knob {
    left: 14.5px;
  }

  .compare input:focus-visible + .track {
    outline: var(--border-focus) solid var(--accent);
    outline-offset: 1px;
  }

  .export {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-left: auto;
  }
</style>
