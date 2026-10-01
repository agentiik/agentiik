<script lang="ts">
  import type { Snippet } from "svelte";
  import type { Ranged } from "../lib/range.svelte";
  import { described, presets, type Preset } from "../lib/stats";

  // The controls along the top of a statistics page: the range, as a preset or a span a zoom chose,
  // the way back from a zoom, the comparison with the span before where the page's series take one,
  // and what the page exports.
  let { ranged, bucket, comparable = true, children }: { ranged: Ranged; bucket: string | undefined; comparable?: boolean; children?: Snippet } = $props();
</script>

<div class="range">
  <div class="presets" role="group" aria-label="Range">
    {#each Object.entries(presets) as [key, p] (key)}
      <button class="preset" aria-pressed={ranged.range.preset === key} onclick={() => ranged.choose(key as Preset)}>{p.label}</button>
    {/each}
  </div>
  <span class="muted term">{described(ranged.range, bucket)}</span>
  {#if ranged.before.length > 0}<button class="link" onclick={() => ranged.back()}>Back to the range before</button>{/if}
  {#if comparable}
    <label class="compare">
      <input type="checkbox" role="switch" checked={ranged.range.compare} onchange={(e) => ranged.compare(e.currentTarget.checked)} />
      <span class="track" aria-hidden="true"><span class="knob"></span></span>
      Compare with the span before
    </label>
  {/if}
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
    max-width: 100%;
    overflow-x: auto;
    gap: calc(var(--unit) * 1);
    padding: 2px;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
  }

  .preset {
    flex: none;
    /* The frame around the presets is a control high: 2px of padding and a hairline on each side. */
    height: calc(var(--control-height) - 6px);
    white-space: nowrap;
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
    width: 30px;
    height: 18px;
    border-radius: var(--radius-round);
    background: var(--lineStrong);
  }

  .knob {
    position: absolute;
    top: 3px;
    left: 3px;
    width: 12px;
    height: 12px;
    border-radius: var(--radius-round);
    background: var(--raised);
  }

  .compare input:checked + .track {
    background: var(--accent);
  }

  .compare input:checked + .track .knob {
    left: 15px;
  }

  .compare input:focus-visible + .track {
    outline: var(--border-focus) solid var(--accent);
    outline-offset: 1px;
  }

  .export {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    max-width: 100%;
    margin-left: auto;
  }
</style>
