<script lang="ts">
  // A state, as every screen of the console draws it: an outlined pill with a dot and the state's own
  // word, so that no state is told by its colour alone. queued, skipped and cancelled are faint rather
  // than a colour of the ramp, since nothing is happening to them; running turns its dot into a ring
  // that spins while the console is live.
  //
  // A step's verdict and a task's state are drawn the same way, in the same words the API writes:
  // dispatched and publishing are a task on its way, and lost is one the infrastructure lost.
  export type State = string;

  let { state, live = true }: { state: State; live?: boolean } = $props();

  const tones: Record<string, string> = {
    running: "running",
    dispatched: "running",
    publishing: "running",
    waiting: "waiting",
    succeeded: "succeeded",
    failed: "failed",
    timed_out: "failed",
    lost: "failed",
  };
  const tone = $derived(tones[state] ?? "quiet");
  const going = $derived(tone === "running");
</script>

<span class="pill {tone}">
  {#if going}
    <span class="ring" class:spinning={live} aria-hidden="true"></span>
  {:else}
    <span class="dot" class:hollow={state === "cancelled" || state === "skipped"} aria-hidden="true"></span>
  {/if}
  <span class="word">{state}</span>
</span>

<style>
  .pill {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    height: 21px;
    padding: 0 calc(var(--unit) * 4) 0 calc(var(--unit) * 4);
    border: var(--border-hairline) solid;
    border-radius: var(--radius-pill);
    font-family: var(--type-identifier-font);
    font-size: 11.5px;
    line-height: 1;
    white-space: nowrap;
  }

  .word {
    padding-top: 1px;
  }

  .dot,
  .ring {
    width: 7px;
    height: 7px;
    border-radius: var(--radius-round);
    flex: none;
  }

  .dot {
    background: currentColor;
  }

  .dot.hollow {
    background: transparent;
    border: 1.5px solid currentColor;
  }

  .ring {
    border: 1.5px solid currentColor;
    border-right-color: transparent;
  }

  .ring.spinning {
    animation: spin 0.9s linear infinite;
  }

  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .ring.spinning {
      animation: none;
    }
  }

  .succeeded {
    color: var(--succeeded);
    background: var(--succeededFill);
    border-color: var(--succeededLine);
  }

  .running {
    color: var(--running);
    background: var(--accentDim);
    border-color: var(--accentLine);
  }

  .waiting {
    color: var(--waiting);
    background: var(--waitingFill);
    border-color: var(--waitingLine);
  }

  .failed {
    color: var(--failed);
    background: var(--failedFill);
    border-color: var(--failedLine);
  }

  .quiet {
    color: var(--faint);
    background: var(--surface);
    border-color: var(--line);
  }
</style>
