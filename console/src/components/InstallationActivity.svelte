<script lang="ts">
  import { untrack } from "svelte";
  import { refusal, type API } from "../api/client";
  import { chartOf, created, hour, paced, type Activity } from "../lib/installation";
  import { useLive } from "../lib/live.svelte";
  import { explain, type Explained } from "../lib/problem";
  import Chart from "./Chart.svelte";
  import Pane from "./Pane.svelte";
  import Problem from "./Problem.svelte";

  // What every namespace together is doing, on an administrator's home: what runs now, and the last
  // hour a minute at a time, from GET /api/v1/stats/activity. It moves as the installation does: the
  // live connection tells an administrator of every run that changes anywhere, and of the runners,
  // and the route is read again when told, at most every two seconds however busy the installation
  // is, and once a minute as the hour slides even where nothing changed.
  let { api }: { api: API } = $props();

  let activity = $state<Activity | null>(null);
  let refused = $state<Explained | null>(null);

  async function read() {
    const { from, to } = hour(Date.now());
    try {
      const { data, error, response } = await api.GET("/api/v1/stats/activity", { params: { query: { from, to, bucket: "1m" } } });
      if (data && typeof data !== "string") {
        activity = data;
        refused = null;
      } else refused = explain("load what the installation is doing", refusal(response, error));
    } catch (e) {
      refused = explain("load what the installation is doing", e);
    }
  }

  const changes = useLive();
  $effect(() => {
    const pace = paced(() => void read(), 2000);
    untrack(() => pace.ask());
    const stop = changes.when((c) => c.kind === "activity" || c.kind === "runners", pace.ask);
    // The hour slides by a minute at each minute's turn, when the next bucket starts.
    const slide = setInterval(pace.ask, 60_000);
    return () => {
      stop();
      clearInterval(slide);
      pace.stop();
    };
  });

  const drawn = $derived(activity ? chartOf(activity) : null);
  const count = (n: number) => (Number.isInteger(n) ? String(n) : "");
  const now = $derived(activity?.now);
  const shown = (n: number | undefined) => (n === undefined ? "\u00a0" : String(n));
</script>

<Pane title="Server activity" aside={activity ? `${created(activity)} runs in the last hour` : ""}>
  {#if refused && !activity}
    <Problem explained={refused} onretry={read} />
  {:else}
    <!-- Drawn at their size before the route answers, each value said once it has. -->
    <ul class="now" aria-label="Now">
      <li><span class="label">Runs running</span><span class="value term">{shown(now?.runs.running)}</span></li>
      <li><span class="label">Runs queued</span><span class="value term">{shown(now?.runs.queued)}</span></li>
      <li><span class="label">Runs awaiting approval</span><span class="value term">{shown(now?.runs.waiting)}</span></li>
      <li><span class="label">Tasks running</span><span class="value term" class:full={now !== undefined && now.slots > 0 && now.tasks_in_flight >= now.slots}>{now ? `${now.tasks_in_flight} / ${now.slots}` : "\u00a0"}</span></li>
      <li><span class="label">Runners ready</span><span class="value term" class:failed={now !== undefined && now.runners > 0 && now.runners_ready === 0}>{now ? `${now.runners_ready} / ${now.runners}` : "\u00a0"}</span></li>
    </ul>
    {#if refused}<Problem explained={refused} onretry={read} />{/if}
    {#if drawn && activity}
      <Chart
        title="Last hour"
        since={drawn.since}
        width={60}
        series={drawn.series}
        stacked
        limit={activity.now.slots > 0 ? { label: "capacity", value: activity.now.slots } : undefined}
        format={count}
        height={150}
      />
    {:else}
      <div class="unread" aria-hidden="true"></div>
    {/if}
  {/if}
</Pane>

<style>
  .now {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(128px, 1fr));
    gap: calc(var(--unit) * 6);
    margin: 0 0 calc(var(--unit) * 8);
    padding: 0;
    list-style: none;
  }

  .now li {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 1);
  }

  .label {
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 500;
  }

  .value {
    min-height: 32px;
    font-size: 24px;
    font-weight: 600;
    line-height: 32px;
  }

  .value.full {
    color: var(--waiting);
  }

  .value.failed {
    color: var(--failed);
  }

  /* The chart's room, kept while the route has not answered, so that nothing moves when it does. */
  .unread {
    height: 150px;
  }
</style>
