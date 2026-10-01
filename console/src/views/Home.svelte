<script lang="ts">
  import { untrack } from "svelte";
  import type { API, Me, Namespace } from "../api/client";
  import Avatar from "../components/Avatar.svelte";
  import { localTime, photoOf } from "../lib/profile";
  import Icon from "../components/Icon.svelte";
  import InstallationActivity from "../components/InstallationActivity.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import { added, boundsOf, dayOf, failures, grid, lastWeek, months, said, shades, together, total, weeks, yearOf, type RunsSeries, type Square } from "../lib/activity";
  import { clock, took } from "../lib/format";
  import { useKeys } from "../lib/keys.svelte";
  import { holdsSomewhereIn, ordered } from "../lib/permissions";
  import { useLive } from "../lib/live.svelte";
  import { paced } from "../lib/installation";
  import { follow, type Place } from "../lib/place.svelte";
  import type { Run } from "../lib/runs.svelte";

  // The console's root, laid out as a forge's home is: who you are, four counts, what needs you, a
  // year of activity and the latest runs, with quick access and, to an administrator, the server's
  // activity beside them. Every namespace the caller reads together, since what a person looks at
  // first is what moved anywhere they work. Nothing here is a route of its own: the squares are each
  // namespace's series by the day, added together, and the runs are GET /api/v1/runs, which lists
  // every namespace's at once.
  let { api, place, me, namespaces }: { api: API; place: Place; me: Me; namespaces: Namespace[] } = $props();

  const now = Date.now();
  const year = yearOf(now);

  // The namespaces read here: those the caller reads runs in, the personal one first, as the switcher
  // lists them.
  const read = $derived.by(() => {
    const { own, personal, shared } = ordered(namespaces, me.principal);
    return [...(own ? [own] : []), ...personal, ...shared].filter((n) => holdsSomewhereIn(me, "run:read", n.name));
  });

  let series = $state(new Map<string, RunsSeries>());
  let unread = $state<string[]>([]);
  let reading = $state(true);
  let recent = $state<Run[]>([]);

  async function readAll(names: string[]) {
    reading = true;
    const answers = await Promise.all(
      names.map(async (ns) => {
        const { data } = await api.GET("/api/v1/{ns}/stats/runs", {
          params: { path: { ns }, query: { from: year.from.toISOString(), to: year.to.toISOString(), bucket: "1d" } },
        });
        return [ns, data] as const;
      }),
    );
    const got = new Map<string, RunsSeries>();
    const missed: string[] = [];
    for (const [ns, data] of answers) {
      if (data) got.set(ns, data as RunsSeries);
      else missed.push(ns);
    }
    series = got;
    unread = missed;
    reading = false;
  }

  // The runs that need the caller: those that failed or timed out in the last day, and those waiting
  // for an approval; and those running now. Each listing is narrowed again here by its state and day,
  // whatever the answer holds.
  let failing = $state<Run[]>([]);
  let waiting = $state<Run[]>([]);
  let running = $state<Run[]>([]);
  const dayAgo = new Date(now - 86_400_000).toISOString();

  async function listed(state: "failed" | "timed_out" | "waiting" | "running", since?: string): Promise<Run[]> {
    const { data } = await api.GET("/api/v1/runs", { params: { query: { state, limit: 100, ...(since ? { since } : {}) } } });
    return (data?.runs ?? []).filter((r) => r.state === state && (!since || r.created_at >= since));
  }

  async function readRecent() {
    const [last, failed, timedOut, approval, under] = await Promise.all([
      api.GET("/api/v1/runs", { params: { query: { limit: 50 } } }).then(({ data }) => data?.runs ?? []),
      listed("failed", dayAgo),
      listed("timed_out", dayAgo),
      listed("waiting"),
      listed("running"),
    ]);
    recent = last;
    failing = [...failed, ...timedOut].sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
    waiting = approval;
    running = under;
  }

  // Read again as runs change anywhere the caller reads, at most every two seconds.
  const changes = useLive();
  $effect(() => {
    const pace = paced(() => void readRecent(), 2000);
    const stop = changes.when((c) => c.kind === "run", pace.ask);
    return () => {
      stop();
      pace.stop();
    };
  });

  $effect(() => {
    const names = read.map((n) => n.name);
    untrack(() => {
      readAll(names);
      readRecent();
    });
  });

  // What needs the caller, as the filter beside it chooses.
  let needs = $state<"all" | "failed" | "waiting">("all");
  const attention = $derived(needs === "failed" ? failing : needs === "waiting" ? waiting : [...waiting, ...failing]);

  // Quick access: the namespaces read, or the workflows the latest runs name, most recent first.
  let quick = $state<"namespaces" | "workflows">("namespaces");
  const workflows = $derived.by(() => {
    const seen = new Map<string, Run>();
    for (const r of recent) if (!seen.has(`${r.namespace}/${r.workflow}`)) seen.set(`${r.namespace}/${r.workflow}`, r);
    return [...seen.values()].slice(0, 8);
  });
  const workflowOf = (r: Run) => ({ kind: "namespace" as const, namespace: r.namespace, view: "workflows" as const, workflow: r.workflow });

  // What shades the squares: every run, or the runs that failed or timed out.
  const by = $derived(place.query.get("by") === "failures" ? "failures" : "runs");
  const counts = $derived(added([...series.values()]));
  const columns = $derived(grid(now, counts));
  const valueOf = (s: Square) => (by === "runs" ? total(s.counts) : failures(s.counts));
  const scale = $derived(shades(columns.flat().filter((s) => !s.future).map(valueOf)));
  const labels = $derived(months(columns));
  const yearTotal = $derived(columns.flat().reduce((n, s) => n + valueOf(s), 0));

  // The day chosen, as ?day= in the address, and its runs across every namespace.
  const chosen = $derived.by(() => {
    const d = place.query.get("day");
    return d && /^\d{4}-\d{2}-\d{2}$/.test(d) ? columns.flat().find((s) => s.day === d && !s.future) : undefined;
  });
  let ofDay = $state<{ day: string; runs: Run[]; more: boolean } | null>(null);
  $effect(() => {
    const d = chosen?.day;
    if (!d) {
      ofDay = null;
      return;
    }
    untrack(async () => {
      const { since, until } = boundsOf(d);
      // One more than is shown, to know whether the day holds more.
      const { data } = await api.GET("/api/v1/runs", { params: { query: { since, until, limit: 101 } } });
      const runs = data?.runs ?? [];
      if (chosen?.day === d) ofDay = { day: d, runs: runs.slice(0, 100), more: runs.length > 100 };
    });
  });

  function choose(day: string | undefined) {
    const q = new URLSearchParams(place.query);
    if (day) q.set("day", day);
    else q.delete("day");
    place.narrow(q);
  }

  function shadeBy(value: "runs" | "failures") {
    const q = new URLSearchParams(place.query);
    if (value === "failures") q.set("by", "failures");
    else q.delete("by");
    place.narrow(q);
  }

  // The arrows move the day chosen along the grid as it is drawn: up and down a day, left and right a
  // week, never onto a day to come.
  const days = $derived(columns.flat().filter((s) => !s.future).map((s) => s.day));
  useKeys(() => [
    {
      keys: ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"],
      brief: ["ArrowLeft", "ArrowRight"],
      effect: "Day",
      does: (key) => {
        const at = chosen ? days.indexOf(chosen.day) : days.length;
        const step = key === "ArrowUp" ? -1 : key === "ArrowDown" ? 1 : key === "ArrowLeft" ? -7 : 7;
        const next = Math.max(0, Math.min(days.length - 1, at + step));
        if (!chosen && (key === "ArrowLeft" || key === "ArrowUp")) choose(days[days.length - 1]);
        else choose(days[next]);
      },
    },
    ...(chosen ? [{ keys: ["Escape"], effect: "Close the day", does: () => choose(undefined) }] : []),
  ]);

  const runRoute = (r: Run) => ({ kind: "namespace" as const, namespace: r.namespace, view: "runs" as const, run: r.run });
  const runsOf = (ns: string) => ({ kind: "namespace" as const, namespace: ns, view: "runs" as const });
  const lasted = (r: Run) => (r.started_at ? took(Math.max(0, (r.finished_at ? Date.parse(r.finished_at) : now) - Date.parse(r.started_at))) : "");
  const weekdays = ["Mon", "", "Wed", "", "Fri", "", ""];
  const name = $derived(me.user?.display_name ?? me.principal);
  // The time where the caller is, where their profile names a zone.
  const here = $derived(localTime(me.user?.timezone ?? "", now));

  // Where the year is wider than its pane, on a phone, it opens on its latest weeks, as a calendar
  // opens on today.
  let yearBox = $state<HTMLDivElement | undefined>();
  $effect(() => {
    if (yearBox) yearBox.scrollLeft = yearBox.scrollWidth;
  });

  // The figures under the caller's name: the last seven days of every namespace read, added up.
  const week = $derived(together(read.map((n) => series.get(n.name)).filter((s) => s !== undefined).map((s) => lastWeek(s, now))));
</script>

<header class="profile">
  <Avatar {name} src={photoOf(me)} size={64} />
  <div class="who">
    <h1>{name}</h1>
    <p class="muted">
      <span class="term">{me.principal}</span>
      {#if me.user?.title}<span>{me.user.title}</span>{/if}
      {#if me.user?.location}<span>{me.user.location}</span>{/if}
      {#if here}<span class="term">{here}</span>{/if}
      {#if me.admin}<span class="role">administrator</span>{/if}
    </p>
  </div>
</header>

<div class="home">
  <div class="main">
    <!-- Drawn at their size before the series answer, each value said once they have. -->
    <ul class="cards" aria-label="Counts">
      <li>
        <span class="head"><span>Runs</span><Icon name="control-runs" size={16} /></span>
        <span class="value term">{reading ? "\u00a0" : week.runs}</span>
        <span class="caption">last 7 days</span>
      </li>
      <li>
        <span class="head"><span>Failed</span><Icon name="state-failed" size={16} /></span>
        <span class="value term" class:failed={!reading && week.failures > 0}>{reading ? "\u00a0" : week.failures}</span>
        <span class="caption">last 7 days</span>
      </li>
      <li>
        <span class="head"><span>Running</span><Icon name="state-running" size={16} /></span>
        <span class="value term">{running.length}</span>
        <span class="caption">now</span>
      </li>
      <li>
        <span class="head"><span>Awaiting approval</span><Icon name="state-waiting" size={16} /></span>
        <span class="value term" class:waiting={waiting.length > 0}>{waiting.length}</span>
        <span class="caption">now</span>
      </li>
    </ul>

    <Pane title="Needs your attention">
      {#snippet actions()}
        <select class="control" bind:value={needs} aria-label="Show">
          <option value="all">Everything</option>
          <option value="failed">Failed, last day</option>
          <option value="waiting">Awaiting approval</option>
        </select>
      {/snippet}
      {#if attention.length === 0}
        <p class="clear"><span class="tick"><Icon name="state-succeeded" size={24} /></span><strong>Nothing needs your attention.</strong></p>
      {:else}
        <ul class="feed" aria-label="Runs that need your attention">
          {#each attention.slice(0, 8) as r (r.run)}
            <li>
              <StatePill state={r.state} />
              <span class="line">
                <a class="term" href={place.href(runRoute(r))} onclick={follow(place, runRoute(r))}>{r.namespace}/{r.workflow}</a>
                <span class="meta"><span class="code">{r.run.slice(-8)}</span> · <span class="term">{r.trigger_kind}</span> by <span class="term">{r.triggered_by}</span></span>
              </span>
              <time class="when term" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time>
            </li>
          {/each}
        </ul>
      {/if}
    </Pane>

    <Pane title="Activity" aside={reading ? "" : `${yearTotal} ${by === "runs" ? (yearTotal === 1 ? "run" : "runs") : yearTotal === 1 ? "failure" : "failures"} in 12 months`}>
      {#snippet actions()}
        <div class="by" role="group" aria-label="What shades the squares">
          <button class="tab" aria-pressed={by === "runs"} onclick={() => shadeBy("runs")}>Runs</button>
          <button class="tab" aria-pressed={by === "failures"} onclick={() => shadeBy("failures")}>Failures</button>
        </div>
      {/snippet}
      {#if unread.length}<p class="refused" role="alert">Could not load the runs of {unread.join(", ")}.</p>{/if}
      <div class="year" class:failures={by === "failures"}>
        <div class="weeks" bind:this={yearBox}>
          <div class="months" style:grid-template-columns="repeat({weeks}, var(--square))" aria-hidden="true">
            {#each labels as m (m.column)}<span style:grid-column="{m.column + 1} / span 3">{m.name}</span>{/each}
          </div>
          <div class="body">
            <div class="weekdays" aria-hidden="true">
              {#each weekdays as w, i (i)}<span>{w}</span>{/each}
            </div>
            <div class="squares" role="grid" aria-label="Runs a day over the last year">
              {#each columns as column, w (w)}
                <div class="week" role="row">
                  {#each column as s (s.day)}
                    {#if s.future}
                      <span class="square future" role="gridcell" aria-hidden="true"></span>
                    {:else}
                      <button
                        class="square level-{scale.level(valueOf(s))}"
                        class:chosen={chosen?.day === s.day}
                        role="gridcell"
                        title={said(s, by)}
                        aria-label={said(s, by)}
                        aria-selected={chosen?.day === s.day}
                        onclick={() => choose(chosen?.day === s.day ? undefined : s.day)}
                      ></button>
                    {/if}
                  {/each}
                </div>
              {/each}
            </div>
          </div>
        </div>
        <div class="legend" aria-hidden="true">
          <span class="faint less">Less</span>
          {#each [0, 1, 2, 3, 4] as l (l)}<span class="square level-{l}"></span>{/each}
          <span class="faint">More</span>
        </div>
      </div>

      {#if chosen}
        <section class="day" aria-label="Runs of {chosen.day}">
          <header>
            <strong>{said(chosen, "runs")}</strong>
            <button class="control" onclick={() => choose(undefined)}><Icon name="control-close" size={14} />Close</button>
          </header>
          {#if ofDay?.day === chosen.day}
            <table>
              <thead><tr><th>State</th><th>Run</th><th>Workflow</th><th>Trigger</th><th>By</th><th>Created</th><th class="number">Took</th></tr></thead>
              <tbody>
                {#each ofDay.runs as r (r.run)}
                  <tr>
                    <td><StatePill state={r.state} /></td>
                    <td class="code"><a href={place.href(runRoute(r))} onclick={follow(place, runRoute(r))}>{r.run}</a></td>
                    <td class="term">{r.namespace}/{r.workflow}</td>
                    <td class="term muted">{r.trigger_kind}</td>
                    <td class="term">{r.triggered_by}</td>
                    <td class="term"><time datetime={r.created_at} title={r.created_at}>{r.created_at.slice(11, 19)} UTC</time></td>
                    <td class="term number">{lasted(r)}</td>
                  </tr>
                {:else}
                  <tr><td colspan="7" class="muted">No runs</td></tr>
                {/each}
              </tbody>
            </table>
            {#if ofDay.more}<p class="faint">First 100 runs</p>{/if}
          {:else}
            <p class="muted" role="status">Loading</p>
          {/if}
        </section>
      {/if}
    </Pane>

    <Pane title="Latest runs">
      <ul class="feed" aria-label="Latest runs">
        {#each recent.slice(0, 10) as r (r.run)}
          <li>
            <StatePill state={r.state} live={r.state === "running"} />
            <span class="line">
              <a class="term" href={place.href(runRoute(r))} onclick={follow(place, runRoute(r))}>{r.namespace}/{r.workflow}</a>
              <span class="meta"><span class="code">{r.run.slice(-8)}</span> · <span class="term">{r.trigger_kind}</span> by <span class="term">{r.triggered_by}</span>{#if lasted(r)}{" · "}<span class="term">{lasted(r)}</span>{/if}</span>
            </span>
            <time class="when term" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time>
          </li>
        {:else}
          <li class="muted">No runs</li>
        {/each}
      </ul>
    </Pane>
  </div>

  <aside class="side">
    {#if me.admin}<InstallationActivity {api} />{/if}

    <Pane title="Quick access">
      <div class="segmented" role="group" aria-label="Quick access">
        <button aria-pressed={quick === "namespaces"} onclick={() => (quick = "namespaces")}>Namespaces</button>
        <button aria-pressed={quick === "workflows"} onclick={() => (quick = "workflows")}>Workflows</button>
      </div>
      <ul class="links">
        {#if quick === "namespaces"}
          {#each read as n (n.name)}
            {@const s = series.get(n.name)}
            {@const seven = s ? lastWeek(s, now) : undefined}
            <li>
              <Icon name="control-namespaces" size={16} />
              <a class="term" href={place.href(runsOf(n.name))} onclick={follow(place, runsOf(n.name))}>{n.name}</a>
              <span class="faint">{seven ? `${seven.runs} ${seven.runs === 1 ? "run" : "runs"}` : ""}</span>
            </li>
          {:else}
            <li class="muted">No namespaces</li>
          {/each}
        {:else}
          {#each workflows as r (`${r.namespace}/${r.workflow}`)}
            <li>
              <Icon name="control-workflows" size={16} />
              <a class="term" href={place.href(workflowOf(r))} onclick={follow(place, workflowOf(r))}>{r.namespace}/{r.workflow}</a>
              <StatePill state={r.state} />
            </li>
          {:else}
            <li class="muted">No workflows</li>
          {/each}
        {/if}
      </ul>
    </Pane>
  </aside>
</div>

<style>
  .profile {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 8);
    margin: 0 0 calc(var(--unit) * 10);
  }

  .who h1 {
    margin: 0;
    font-family: var(--type-pageTitle-font);
    font-size: 22px;
    font-weight: var(--type-pageTitle-weight);
    --leading: 1.25;
  }

  .who p {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 2) calc(var(--unit) * 6);
    margin: calc(var(--unit) * 1) 0 0;
  }

  .who p span {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
  }

  .role {
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-chip);
    font-size: 12.5px;
  }

  /* Two columns, as a forge's home has: what is the caller's on the left, quick access and the
     server beside it; one above the other where the window is narrow. */
  .home {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(300px, 360px);
    align-items: start;
    gap: calc(var(--unit) * 8);
  }

  .main,
  .side {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 8);
    min-width: 0;
  }

  /* Four counts, each a card with its name and icon in a band over its value. */
  .cards {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: calc(var(--unit) * 6);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .cards li {
    display: flex;
    flex-direction: column;
    overflow: hidden;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-card);
    background: var(--surface);
  }

  .cards .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: calc(var(--unit) * 3);
    height: 36px;
    padding: 0 calc(var(--unit) * 6);
    background: var(--sunken);
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 500;
  }

  .cards .value {
    padding: calc(var(--unit) * 4) calc(var(--unit) * 6) 0;
    font-size: 28px;
    font-weight: 600;
    line-height: 36px;
  }

  .cards .value.failed {
    color: var(--failed);
  }

  .cards .value.waiting {
    color: var(--waiting);
  }

  .cards .caption {
    padding: 0 calc(var(--unit) * 6) calc(var(--unit) * 5);
    color: var(--faint);
    font-size: 12.5px;
  }

  .clear {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 6);
    margin: calc(var(--unit) * 2) 0;
  }

  .tick {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 48px;
    height: 48px;
    border: 2px solid var(--text);
    border-radius: var(--radius-round);
    background: color-mix(in srgb, var(--succeeded) 22%, var(--surface));
    color: var(--succeeded);
  }

  /* A feed of runs: its state, what it is on one line and how it came on the next, and when. */
  .feed {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .feed li {
    display: grid;
    grid-template-columns: 120px minmax(0, 1fr) auto;
    align-items: start;
    gap: calc(var(--unit) * 6);
    padding: calc(var(--unit) * 4) 0;
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
  }

  .feed li:last-child {
    box-shadow: none;
  }

  .line {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .line a {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .meta,
  .when {
    color: var(--muted);
    font-size: 12.5px;
  }

  .when {
    white-space: nowrap;
  }

  .segmented {
    display: grid;
    grid-template-columns: 1fr 1fr;
    margin-bottom: calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    overflow: hidden;
  }

  .segmented button {
    height: var(--control-height);
    border: none;
    background: var(--surface);
    color: var(--muted);
    font-size: var(--type-control-size);
    cursor: pointer;
  }

  .segmented button + button {
    box-shadow: inset var(--border-hairline) 0 0 var(--lineStrong);
  }

  .segmented button[aria-pressed="true"] {
    background: var(--sunken);
    color: var(--text);
    font-weight: 500;
  }

  .links {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .links li {
    display: grid;
    grid-template-columns: 16px minmax(0, 1fr) auto;
    align-items: center;
    gap: calc(var(--unit) * 4);
    min-height: 36px;
    color: var(--muted);
  }

  .links a {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .links .faint {
    font-size: 12.5px;
  }

  .by {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
  }

  .tab {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid transparent;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    font-size: var(--type-navigation-size);
    font-weight: 500;
    cursor: pointer;
  }

  .tab[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .year {
    --square: 11px;
    --gap: 3px;
    --shade: var(--accent);
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 2);
    width: max-content;
    max-width: 100%;
    font-size: var(--type-identifier-size-min);
  }

  /* The weeks alone scroll where the year is wider than the pane, its legend staying in view, with
     room inside for the ring of the day chosen, which the scrolling box would otherwise cut. */
  .weeks {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 2);
    overflow: auto hidden;
    padding: 4px;
  }

  .year.failures {
    --shade: var(--failed);
  }

  .months {
    display: grid;
    column-gap: var(--gap);
    margin-left: 34px;
    color: var(--faint);
  }

  .body {
    display: flex;
    gap: 6px;
  }

  .weekdays {
    display: grid;
    grid-template-rows: repeat(7, var(--square));
    row-gap: var(--gap);
    width: 28px;
    color: var(--faint);
  }

  .weekdays span {
    line-height: var(--square);
  }

  .squares {
    display: flex;
    gap: var(--gap);
  }

  .week {
    display: grid;
    grid-template-rows: repeat(7, var(--square));
    row-gap: var(--gap);
  }

  .square {
    width: var(--square);
    height: var(--square);
    padding: 0;
    border: var(--border-hairline) solid color-mix(in srgb, var(--text) 6%, transparent);
    border-radius: 2px;
    background: var(--sunken);
    cursor: pointer;
  }

  .square.future {
    border-color: transparent;
    background: none;
    cursor: default;
  }

  .square.chosen,
  .square:focus-visible {
    outline: var(--border-focus) solid var(--text);
    outline-offset: 1px;
  }

  .level-1 {
    background: color-mix(in srgb, var(--shade) 22%, var(--sunken));
  }

  .level-2 {
    background: color-mix(in srgb, var(--shade) 44%, var(--sunken));
  }

  .level-3 {
    background: color-mix(in srgb, var(--shade) 68%, var(--sunken));
  }

  .level-4 {
    background: var(--shade);
  }

  .legend {
    display: flex;
    align-items: center;
    gap: var(--gap);
    margin-top: calc(var(--unit) * 2);
  }



  .legend .square {
    cursor: default;
  }

  .legend .faint {
    margin: 0 calc(var(--unit) * 2);
  }

  .legend .faint.less {
    margin-left: auto;
  }

  .day {
    margin-top: calc(var(--unit) * 7);
  }

  .day header {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    margin-bottom: calc(var(--unit) * 4);
    font-size: var(--type-navigation-size);
  }

  .day header .control {
    margin-left: auto;
  }

  .refused {
    margin: 0 0 calc(var(--unit) * 4);
    color: var(--failed);
    font-size: var(--type-control-size);
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th {
    height: var(--row-header);
    padding: 0 calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-align: left;
    text-transform: var(--type-columnHead-case);
  }

  td {
    height: var(--row-body);
    padding: 0 calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    font-size: var(--type-identifier-size-max);
    white-space: nowrap;
  }

  td a {
    color: var(--accent);
  }

  .number {
    text-align: right;
  }

  /* Under 1100px, where the sidebar folds, the side column goes under the main one, and the four
     counts two by two. */
  @media (max-width: 1099px) {
    .home {
      grid-template-columns: minmax(0, 1fr);
    }

    .cards {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }

  @media (max-width: 759px) {
    .profile {
      gap: calc(var(--unit) * 6);
    }

    .cards .value {
      font-size: 22px;
    }

    .feed li {
      grid-template-columns: minmax(0, 1fr) auto;
    }

    .feed li > :global(:first-child) {
      grid-column: 1 / -1;
      justify-self: start;
    }
  }
</style>
