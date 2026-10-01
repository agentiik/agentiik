<script lang="ts">
  import { onMount, untrack } from "svelte";
  import type { API } from "./api/client";
  import KeyLine from "./components/KeyLine.svelte";
  import Problem from "./components/Problem.svelte";
  import Pane from "./components/Pane.svelte";
  import Sidebar from "./components/Sidebar.svelte";
  import TopBar from "./components/TopBar.svelte";
  import { Viewport } from "./lib/viewport.svelte";
  import { Keys, provide } from "./lib/keys.svelte";
  import { Live, provideLive } from "./lib/live.svelte";
  import { holds, holdsSomewhereIn, inNamespace } from "./lib/permissions";
  import type { Place } from "./lib/place.svelte";
  import type { View } from "./lib/route";
  import type { Session } from "./lib/session.svelte";
  import { firstNamespace, fold, folded as wasFolded, keepNamespace, lastNamespace } from "./lib/shell";
  import Account from "./views/Account.svelte";
  import Fleet from "./views/Fleet.svelte";
  import Groups from "./views/Groups.svelte";
  import Home from "./views/Home.svelte";
  import Namespaces from "./views/Namespaces.svelte";
  import PoolStatistics from "./views/PoolStatistics.svelte";
  import Refused from "./views/Refused.svelte";
  import Run from "./views/Run.svelte";
  import RunDiff from "./views/RunDiff.svelte";
  import Runs from "./views/Runs.svelte";
  import Settings from "./views/Settings.svelte";
  import Sharing from "./views/Sharing.svelte";
  import SignIn, { type Passkeys } from "./views/SignIn.svelte";
  import Statistics from "./views/Statistics.svelte";
  import Users from "./views/Users.svelte";
  import Workflow from "./views/Workflow.svelte";
  import Workflows from "./views/Workflows.svelte";
  import WorkflowStatistics from "./views/WorkflowStatistics.svelte";

  // The console: who it is signed in as, the top bar, the screen the address names, and the key line.
  let { api, session, place, version, passkeys }: { api: API; session: Session; place: Place; version: string; passkeys: Passkeys } = $props();

  onMount(() => {
    session.read();
  });

  // The live connection, open while somebody is signed in: each screen reads again what it shows
  // when told it changed, the caller's notifications are read again here, and a connection the API
  // closes because its credential is gone reads who the console is signed in as.
  const live = new Live(untrack(() => place.baseURI), () => session.read());
  provideLive(live);
  $effect(() => {
    if (session.standing !== "signed-in") return;
    live.start();
    const reading = live.when((c) => c.kind === "notifications", () => session.read());
    return () => {
      reading();
      live.stop();
    };
  });

  const route = $derived(place.route);
  const namespace = $derived(route.kind === "namespace" ? route.namespace : undefined);

  // The namespace whose views the sidebar lists: the one the screen is in, or else the one last
  // opened, or else the caller's own, so that the sidebar keeps its entries on the home and the account.
  let remembered = $state(lastNamespace(globalThis.localStorage));
  $effect(() => {
    if (namespace && session.namespaces.some((n) => n.name === namespace)) {
      keepNamespace(globalThis.localStorage, namespace);
      remembered = namespace;
    }
  });
  // A namespace the caller cannot read is never listed, so that an address naming one draws the same
  // sidebar as one naming nothing.
  const context = $derived(
    namespace && session.namespaces.some((n) => n.name === namespace)
      ? namespace
      : remembered && session.namespaces.some((n) => n.name === remembered)
        ? remembered
        : session.me
          ? firstNamespace(session.me, session.namespaces)
          : undefined,
  );
  let sidebarFolded = $state(wasFolded(globalThis.localStorage));
  function foldSidebar(value: boolean) {
    sidebarFolded = value;
    fold(globalThis.localStorage, value);
  }

  // The frame as the window's width lays it out (lib/viewport): the sidebar folded under 1100px
  // whatever its reader chose, and under 760px a drawer, whole, opened from the top bar and closed
  // again by the screen it leads to, by esc or by a click beside it.
  const viewport = new Viewport();
  const folded = $derived(!viewport.narrow && (viewport.compact || sidebarFolded));
  let drawer = $state(false);
  $effect(() => {
    void route;
    drawer = false;
  });

  // The views of a namespace, each shown to a caller who holds what reading it takes there, and to no
  // other: a view the caller cannot use is left out of the bar rather than drawn disabled.
  const all: { view: View; label: string; shows: (ns: string) => boolean }[] = [
    { view: "runs", label: "Runs", shows: (ns) => (session.me ? holdsSomewhereIn(session.me, "run:read", ns) : false) },
    { view: "workflows", label: "Workflows", shows: (ns) => (session.me ? holdsSomewhereIn(session.me, "workflow:read", ns) : false) },
    { view: "statistics", label: "Statistics", shows: (ns) => (session.me ? inNamespace(session.me, ns) : false) },
    { view: "sharing", label: "Sharing", shows: (ns) => (session.me ? holdsSomewhereIn(session.me, "grant:manage", ns) : false) },
    { view: "settings", label: "Settings", shows: (ns) => (session.me ? inNamespace(session.me, ns) : false) },
  ];

  // Built so far: the views the console draws in this release. The others arrive with theirs.
  const built = new Set<View>(["runs", "workflows", "statistics", "sharing", "settings"]);

  const known = $derived(namespace !== undefined && session.namespaces.some((n) => n.name === namespace));
  const shown = $derived(namespace && known ? all.filter((v) => built.has(v.view) && v.shows(namespace)) : []);
  const listed = $derived(context ? all.filter((v) => built.has(v.view) && v.shows(context)) : []);

  // A workflow's own statistics, the one page of a workflow built so far, open to whoever reads runs
  // somewhere in its namespace: the series answer a workflow the caller cannot read as one that does
  // not exist, so the page says no more than the API does.
  const workflowStatistics = $derived(
    route.kind === "namespace" && route.view === "workflows" && route.workflow !== undefined && route.tab === "statistics" && known && !!session.me && holdsSomewhereIn(session.me, "run:read", route.namespace),
  );

  // The console's own keys, beside those of the view drawn: a digit for each view of the top bar,
  // in its order there, as agk console numbers its views, and ? for every key of the view.
  const keys = provide(
    new Keys(() => [
      ...(namespace && shown.length > 0
        ? [
            {
              keys: shown.map((_, i) => String(i + 1)),
              effect: shown.map((v, i) => (i === 0 ? v.label : v.label.toLowerCase())).join(", "),
              does: (key: string) => {
                const v = shown[Number(key) - 1];
                if (v) place.go({ kind: "namespace", namespace, view: v.view });
              },
            },
          ]
        : []),
      { keys: ["?"], effect: "Every key", does: () => (keys.listing = !keys.listing) },
    ]),
  );
</script>

<svelte:window
  onkeydown={(e) => {
    if (drawer && e.key === "Escape") {
      drawer = false;
      e.preventDefault();
      return;
    }
    if (session.me && session.standing === "signed-in") keys.press(e);
  }}
/>

{#if session.standing === "reading"}
  <!-- The frame is drawn while the session is read, empty, so that nothing moves when it fills. -->
  <div class="frame" class:folded class:narrow={viewport.narrow}>
    <div class="side blank" aria-hidden="true"></div>
    <div class="top blank" aria-hidden="true"></div>
    <main class="screen" aria-busy="true"></main>
    <div class="keys blank" aria-hidden="true"></div>
  </div>
{:else if session.standing === "signed-out"}
  <SignIn {api} {session} {passkeys} />
{:else if session.standing === "enrol-only"}
  <main class="alone">
    <Pane title="Set up a passkey">
      <p>You are signed in, but only to set up a passkey: this installation asks for one before you can do anything else.</p>
      <p><a class="control primary" href="auth/enrol">Set up a passkey</a></p>
    </Pane>
  </main>
{:else if session.standing === "unreachable"}
  <main class="alone">
    <Pane title="Agentiik is not available">
      {#if session.failure}<Problem explained={session.failure} onretry={() => session.read()} />{/if}
    </Pane>
  </main>
{:else if session.me}
  <div class="frame" class:folded class:narrow={viewport.narrow} class:drawn={drawer}>
    <div class="side" inert={viewport.narrow && !drawer}>
      <Sidebar me={session.me} namespaces={session.namespaces} namespace={context} shown={listed} {route} {place} {folded} foldable={!viewport.compact} onfold={foldSidebar} />
    </div>
    {#if viewport.narrow && drawer}
      <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
      <div class="scrim" onclick={() => (drawer = false)}></div>
    {/if}
    <div class="top">
      <TopBar me={session.me} {route} answering={session.answering} live={live.open} {place} onsignout={() => session.signOut()} ondismiss={(id) => session.dismiss(id)} onmenu={viewport.narrow ? () => (drawer = true) : undefined} />
    </div>
    <main class="screen">
      {#if route.kind === "namespace" && route.workflow && workflowStatistics}
        <WorkflowStatistics {api} {place} namespace={route.namespace} workflow={route.workflow} graph={holds(session.me, "workflow:read", route.namespace, route.workflow)} shares={holds(session.me, "grant:manage", route.namespace, route.workflow)} />
      {:else if route.kind === "namespace" && route.view === "workflows" && route.workflow && (route.tab === undefined || route.tab === "graph" || route.tab === "mcp" || route.tab === "files") && known && holdsSomewhereIn(session.me, "workflow:read", route.namespace)}
        <!-- A workflow's page: the API answers one the
             caller cannot read as one that does not exist, and the page says no more. -->
        <Workflow {api} {place} me={session.me} namespace={route.namespace} workflow={route.workflow} tab={route.tab} />
      {:else if route.kind === "namespace" && (!known || !shown.some((v) => v.view === route.view))}
        <Refused />
      {:else if route.kind === "namespace" && route.view === "runs" && route.run && route.against}
        <RunDiff {api} {place} me={session.me} namespace={route.namespace} a={route.run} b={route.against} />
      {:else if route.kind === "namespace" && route.view === "runs" && route.run}
        <Run {api} {place} me={session.me} namespace={route.namespace} id={route.run} />
      {:else if route.kind === "namespace" && route.view === "runs"}
        <Runs {api} {place} me={session.me} namespace={route.namespace} record={session.namespaces.find((n) => n.name === route.namespace)} />
      {:else if route.kind === "namespace" && route.view === "workflows" && route.workflow === undefined}
        <Workflows {api} {place} me={session.me} namespace={route.namespace} />
      {:else if route.kind === "namespace" && route.view === "sharing"}
        <Sharing {api} {place} me={session.me} namespace={route.namespace} />
      {:else if route.kind === "namespace" && route.view === "settings"}
        <Settings {api} {place} me={session.me} namespace={route.namespace} />
      {:else if route.kind === "namespace" && route.view === "statistics"}
        <Statistics {api} {place} namespace={route.namespace} record={session.namespaces.find((n) => n.name === route.namespace)} />
      {:else if route.kind === "landing"}
        <Home {api} {place} me={session.me} namespaces={session.namespaces} />
      {:else if route.kind === "account"}
        <Account {api} {place} me={session.me} tab={route.tab} {passkeys} changed={() => session.read()} />
      {:else if route.kind === "users" && session.me.admin}
        <Users {api} {place} me={session.me} />
      {:else if route.kind === "groups" && session.me.admin}
        <Groups {api} {place} />
      {:else if route.kind === "namespaces" && session.me.admin}
        <Namespaces {api} {place} changed={() => session.read()} />
      {:else if route.kind === "runners" && route.tab === undefined && session.me.admin}
        <Fleet {api} {place} namespaces={session.namespaces} {version} />
      {:else if route.kind === "runners" && route.tab === "statistics" && session.me.admin}
        <PoolStatistics {api} {place} />
      {:else}
        <Refused />
      {/if}
    </main>
    <div class="keys"><KeyLine {keys} {version} /></div>
  </div>
{/if}

<style>
  .frame {
    display: grid;
    grid-template-columns: var(--sidebar-width) minmax(0, 1fr);
    grid-template-rows: var(--bar-top) minmax(0, 1fr) var(--bar-keyLine);
    grid-template-areas: "side top" "side screen" "side keys";
    height: 100%;
  }

  .frame.folded {
    grid-template-columns: var(--sidebar-collapsed) minmax(0, 1fr);
  }

  .side {
    grid-area: side;
    min-height: 0;
  }

  .top {
    grid-area: top;
  }

  .keys {
    grid-area: keys;
  }

  .frame > .screen {
    grid-area: screen;
  }

  .side.blank {
    border-right: var(--border-hairline) solid var(--line);
    background: var(--surface);
  }

  .top.blank {
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    background: var(--surface);
  }

  .keys.blank {
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
    background: var(--surface);
  }

  .screen {
    min-height: 0;
    padding: calc(var(--padding-page) + 6px) calc(var(--padding-page) + 6px) calc(var(--padding-page) + 12px);
    overflow: auto;
    scrollbar-gutter: stable;
  }

  .alone {
    max-width: 480px;
    margin: 18vh auto 0;
  }

  /* Under 760px the screen takes the window's whole width, the sidebar opens over it as a drawer, and
     the key line is not drawn, since the keys it names are a keyboard's. */
  .frame.narrow {
    grid-template-columns: minmax(0, 1fr);
    grid-template-rows: var(--bar-top) minmax(0, 1fr);
    grid-template-areas: "top" "screen";
  }

  .frame.narrow .keys {
    display: none;
  }

  .frame.narrow .side {
    position: fixed;
    top: 0;
    bottom: 0;
    left: 0;
    z-index: 60;
    transform: translateX(-100%);
    transition: transform 0.18s ease;
  }

  .frame.narrow.drawn .side {
    transform: none;
    box-shadow: 0 0 32px rgb(0 0 0 / 0.25);
  }

  .scrim {
    position: fixed;
    inset: 0;
    z-index: 55;
    background: color-mix(in srgb, var(--bg) 55%, transparent);
  }

  @media (max-width: 759px) {
    .screen {
      padding: 16px 16px 24px;
    }

    .alone {
      margin: 10vh 16px 0;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .frame.narrow .side {
      transition: none;
    }
  }

  .alone p {
    margin: 0 0 calc(var(--unit) * 6);
    font-size: var(--type-navigation-size);
  }

  .alone p:last-child {
    margin-bottom: 0;
  }
</style>
