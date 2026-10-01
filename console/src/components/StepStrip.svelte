<script lang="ts">
  import { took } from "../lib/format";

  // A run's steps as one strip, a segment a step in the order they started, each as wide as the step
  // took, or has taken while it runs: where a run failed and how long each part of it lasted are
  // read from the position and the width, before the colour. A step not started yet is a sliver at
  // the end, and the strip says every step in words to whoever reads it rather than sees it.
  type Strip = { step: string; verdict: string; started_at?: string; finished_at?: string };
  let { steps, now }: { steps: Strip[]; now: number } = $props();

  // A step not started weighs as much as a short one, so that it is drawn at all.
  const floor = 1000;

  const segments = $derived(
    steps.map((s) => {
      const from = s.started_at ? Date.parse(s.started_at) : NaN;
      const to = s.finished_at ? Date.parse(s.finished_at) : now;
      const ms = Number.isNaN(from) ? undefined : Math.max(0, to - from);
      return { ...s, ms, weight: Math.max(floor, ms ?? 0) };
    }),
  );

  function said(s: { step: string; verdict: string; ms?: number }): string {
    if (s.ms === undefined) {
      return `${s.step} not reached`;
    }
    return `${s.step} ${s.verdict} in ${took(s.ms)}`;
  }

  const label = $derived(segments.length === 0 ? "No step has been reached" : `Steps: ${segments.map(said).join(", ")}`);
</script>

<span class="strip" role="img" aria-label={label}>
  {#each segments as s (s.step)}
    <span class="segment {s.verdict}" class:unreached={s.ms === undefined} style:flex-grow={s.weight} title={said(s)}></span>
  {/each}
</span>

<style>
  .strip {
    display: flex;
    gap: 2px;
    width: 140px;
    height: 8px;
  }

  .segment {
    flex-basis: 0;
    min-width: 3px;
    border-radius: 2px;
    background: var(--faint);
  }

  .segment.succeeded {
    background: var(--succeeded);
  }

  .segment.failed {
    background: var(--failed);
  }

  .segment.running {
    background: var(--running);
  }

  .segment.unreached,
  .segment.skipped,
  .segment.cancelled {
    background: var(--line);
  }
</style>
