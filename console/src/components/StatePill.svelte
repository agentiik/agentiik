<script lang="ts">
  // A state, as every screen of the console draws it: a soft round pill, a pale fill of its hue with a
  // dot and the state's own word, so that no state is told by its colour alone. queued, skipped and
  // cancelled are muted on sunken with a hollow dot rather than a colour of the ramp, since nothing is
  // happening to them; running's dot breathes while the console is live, the one state that moves.
  //
  // A step's verdict and a task's state are drawn the same way, in the same words the API writes:
  // dispatched and publishing are a task on its way, and lost is one the infrastructure lost. So is a
  // runner's condition: ready takes work, draining finishes what it holds, unhealthy and silent take
  // nothing though nobody ordered them to stop, and revoked is out of service for good.
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
    ready: "succeeded",
    draining: "waiting",
    unhealthy: "failed",
    silent: "failed",
  };
  const tone = $derived(tones[state] ?? "quiet");
  const going = $derived(tone === "running");
</script>

<span class="pill {tone}">
  <span class="dot" class:hollow={tone === "quiet"} class:breathing={going && live} aria-hidden="true"></span>
  <span class="word">{state}</span>
</span>

<style>
  .pill {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    height: 22px;
    padding: 0 calc(var(--unit) * 5) 0 calc(var(--unit) * 4);
    border-radius: var(--radius-pill);
    font-size: 12.5px;
    font-weight: 500;
    --leading: 1;
    white-space: nowrap;
  }

  /* The pill's baseline is its first item's, the dot's, so the dot keeps the 8px box it has always
     had and draws 6px inside a transparent border: a smaller box would move every line holding a
     pill by half a pixel. */
  .dot {
    width: 8px;
    height: 8px;
    border: 1px solid transparent;
    border-radius: var(--radius-round);
    flex: none;
    background: currentColor;
    background-clip: padding-box;
  }

  .dot.hollow {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px currentColor;
  }

  /* A ring that grows from the dot and fades, once every 1.6s: slow enough to read as breathing
     rather than as an alarm, and drawn outside the pill's box, so that nothing beside it moves. */
  .dot.breathing {
    animation: breathe 1.6s var(--motion-easeOut) infinite;
  }

  @keyframes breathe {
    0% {
      box-shadow: 0 0 0 0 color-mix(in srgb, currentColor 55%, transparent);
    }
    70%,
    100% {
      box-shadow: 0 0 0 6px transparent;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .dot.breathing {
      animation: none;
    }
  }

  .succeeded {
    color: var(--succeeded);
    background: var(--succeededFill);
  }

  .running {
    color: var(--running);
    background: var(--accentDim);
  }

  .waiting {
    color: var(--waiting);
    background: var(--waitingFill);
  }

  .failed {
    color: var(--failed);
    background: var(--failedFill);
  }

  /* A faint state is muted on sunken: nothing is happening to it, so it sits below the ground rather
     than on it, and its dot is hollow. */
  .quiet {
    color: var(--muted);
    background: var(--sunken);
  }
</style>
