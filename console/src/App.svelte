<script lang="ts">
  import { onMount, untrack } from "svelte";
  import type { API } from "./api/client";
  import KeyLine from "./components/KeyLine.svelte";
  import Problem from "./components/Problem.svelte";
  import Pane from "./components/Pane.svelte";
  import Sidebar from "./components/Sidebar.svelte";
  import TopBar from "./components/TopBar.svelte";
  import { Viewport } from "./lib/viewport.svelte";
  import { Keys, provide, shown as keyShown } from "./lib/keys.svelte";
  import Palette from "./components/Palette.svelte";
  import type { Entry } from "./lib/palette";
  import { Live, provideLive } from "./lib/live.svelte";
  import { holds, holdsSomewhereIn, inNamespace, ordered } from "./lib/permissions";
  import type { Place } from "./lib/place.svelte";
  import type { View } from "./lib/route";
  import type { Session } from "./lib/session.svelte";
  import { administration, firstNamespace, fold, folded as wasFolded, keepNamespace, lastNamespace, viewIcons } from "./lib/shell";
  import { titleOf } from "./lib/trail";
  import { settles } from "./lib/page";
  import Unloaded from "./components/Unloaded.svelte";
  import Refused from "./views/Refused.svelte";
  import NewNamespace from "./components/NewNamespace.svelte";
  import SignIn, { type Passkeys } from "./views/SignIn.svelte";

  // The console: who it is signed in as, the top bar, the screen the address names, and the key line.
  let { api, session, place, version, passkeys }: { api: API; session: Session; place: Place; version: string; passkeys: Passkeys } = $props();

  onMount(() => {
    session.read();
  });

  // Each screen is a chunk of its own, loaded when it is first shown, so that the console's first load
  // carries the shell and the screen opened rather than every screen there is. A chunk is fetched once
  // and kept by the browser; the build names it by its content, so that a release never serves a stale one.
  const screens = {
    Account: () => import("./views/Account.svelte"),
    Fleet: () => import("./views/Fleet.svelte"),
    Groups: () => import("./views/Groups.svelte"),
    Home: () => import("./views/Home.svelte"),
    Namespaces: () => import("./views/Namespaces.svelte"),
    Policy: () => import("./views/Policy.svelte"),
    Audit: () => import("./views/Audit.svelte"),
    PoolStatistics: () => import("./views/PoolStatistics.svelte"),
    Run: () => import("./views/Run.svelte"),
    RunDiff: () => import("./views/RunDiff.svelte"),
    Runs: () => import("./views/Runs.svelte"),
    Settings: () => import("./views/Settings.svelte"),
    Sharing: () => import("./views/Sharing.svelte"),
    Statistics: () => import("./views/Statistics.svelte"),
    Users: () => import("./views/Users.svelte"),
    Variables: () => import("./views/Variables.svelte"),
    Workflow: () => import("./views/Workflow.svelte"),
    Workflows: () => import("./views/Workflows.svelte"),
    WorkflowStatistics: () => import("./views/WorkflowStatistics.svelte"),
  };

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

  // What a screen shows: its namespace, and its run and the run it is read against, or else its
  // workflow. A screen reads what it shows when it is drawn, so another namespace, workflow or run
  // draws it again rather than leave it showing the one before; a tab, a step chosen or a query
  // keeps it. A run's screen is the run's, whatever workflow the address names, since a run reached
  // by its address of before has its workflow written in once it is read, and is the same run.
  const screen = $derived(
    route.kind === "namespace"
      ? ["namespace", route.namespace, ...(route.run ? ["run", route.run, route.against ?? ""] : ["workflow", route.workflow ?? ""])].join("\u0000")
      : route.kind,
  );

  // The browser's tab names the screen, so that a row of tabs and the history read by what each shows.
  $effect(() => {
    document.title = session.standing === "signed-in" ? titleOf(route) : session.standing === "signed-out" ? "Sign in · Agentiik" : "Agentiik";
  });
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
  // A namespace created from the switcher's foot, then opened once the list of namespaces holds it.
  let creating = $state(false);

  async function created(name: string) {
    await session.read();
    place.go({ kind: "namespace", namespace: name, view: "workflows" });
  }

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
    // Workflows to whoever reads one there or follows the runs of one, an operator among them, since
    // the list is of the workflows its runs name.
    { view: "workflows", label: "Workflows", shows: (ns) => (session.me ? holdsSomewhereIn(session.me, "workflow:read", ns) || holdsSomewhereIn(session.me, "run:read", ns) : false) },
    { view: "statistics", label: "Statistics", shows: (ns) => (session.me ? inNamespace(session.me, ns) : false) },
    { view: "sharing", label: "Sharing", shows: (ns) => (session.me ? holdsSomewhereIn(session.me, "grant:manage", ns) : false) },
    { view: "variables", label: "Variables", shows: (ns) => (session.me ? holds(session.me, "workflow:read", ns) : false) },
    { view: "settings", label: "Settings", shows: (ns) => (session.me ? inNamespace(session.me, ns) : false) },
  ];

  // Built so far: the views the console draws in this release. The others arrive with theirs.
  const built = new Set<View>(["workflows", "statistics", "sharing", "variables", "settings"]);

  const known = $derived(namespace !== undefined && session.namespaces.some((n) => n.name === namespace));
  const shown = $derived(namespace && known ? all.filter((v) => built.has(v.view) && v.shows(namespace)) : []);
  const listed = $derived(context ? all.filter((v) => built.has(v.view) && v.shows(context)) : []);

  // A workflow's own statistics, the one page of a workflow built so far, open to whoever reads runs
  // somewhere in its namespace: the series answer a workflow the caller cannot read as one that does
  // not exist, so the page says no more than the API does.
  const workflowStatistics = $derived(
    route.kind === "namespace" && route.view === "workflows" && route.workflow !== undefined && route.tab === "statistics" && known && !!session.me && holdsSomewhereIn(session.me, "run:read", route.namespace),
  );

  // A workflow's runs, a run and two runs compared, open as its statistics are to whoever reads runs
  // somewhere in the namespace, since the API answers a run the caller cannot read as one that does
  // not exist. A run reached by its address of before has no workflow in it yet.
  const runs = $derived(route.kind === "namespace" && route.view === "workflows" && (route.run !== undefined || (route.workflow !== undefined && route.tab === "runs")) && known && !!session.me && holdsSomewhereIn(session.me, "run:read", route.namespace));

  // A workflow its caller follows the runs of without reading it, as an operator, opens on its runs:
  // the graph is reading it, which the API would answer as a workflow that does not exist.
  const operating = $derived(
    route.kind === "namespace" && route.view === "workflows" && route.workflow !== undefined && route.run === undefined && route.tab === undefined && known && !!session.me && !holds(session.me, "workflow:read", route.namespace, route.workflow) && holdsSomewhereIn(session.me, "run:read", route.namespace),
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
      { keys: [":"], effect: "Search", does: () => (searching = true) },
      { keys: ["?"], effect: "Every key", does: () => (keys.listing = !keys.listing) },
    ]),
  );

  // The palette, which : and Search in the sidebar open: the views of the sidebar, the namespaces and
  // the keys of the screen drawn, to which it adds the workflows and the latest runs it reads as it
  // opens. A key that moves a selection is left out, since the palette has nothing chosen for it to
  // move; so are the digits, whose views are listed themselves.
  let searching = $state(false);
  const moving = new Set(["ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "j", "k", "[", "]"]);
  const entries = $derived.by((): Entry[] => {
    const me = session.me;
    if (!me) return [];
    const { own, personal, shared } = ordered(session.namespaces, me.principal);
    return [
      { id: "view:home", kind: "view", label: "Home", icon: "control-home", to: { kind: "landing" } },
      ...(context ? listed.map((v): Entry => ({ id: `view:${v.view}`, kind: "view", label: v.label, detail: context, icon: viewIcons[v.view], to: { kind: "namespace", namespace: context, view: v.view } })) : []),
      ...(me.admin ? administration.map((a): Entry => ({ id: `view:${a.label}`, kind: "view", label: a.label, detail: "Administration", icon: a.icon, to: a.to })) : []),
      { id: "view:account", kind: "view", label: "Your account", icon: "control-users", to: { kind: "account", tab: "profile" } },
      ...[...(own ? [own] : []), ...personal, ...shared].flatMap((n): Entry[] => {
        const first = all.find((v) => built.has(v.view) && v.shows(n.name));
        return first ? [{ id: `namespace:${n.name}`, kind: "namespace", label: n.name, to: { kind: "namespace", namespace: n.name, view: first.view } }] : [];
      }),
      ...keys.bindings
        .filter((b) => !b.keys.some((k) => moving.has(k) || /^\d$/.test(k)) && !b.keys.includes(":"))
        .map((b): Entry => ({ id: `key:${b.effect}`, kind: "key", label: b.effect, key: keyShown(b.keys[0]!), does: () => b.does(b.keys[0]!) })),
    ];
  });
  const followed = $derived(session.me ? session.namespaces.filter((n) => holdsSomewhereIn(session.me!, "run:read", n.name)) : []);
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
    <main class="screen" aria-busy="true"></main>
  </div>
{:else if session.standing === "signed-out"}
  <SignIn {api} {session} {passkeys} {version} />
{:else if session.standing === "enrol-only"}
  <main class="alone">
    <Pane title="Set up a passkey">
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
      <Sidebar me={session.me} namespaces={session.namespaces} namespace={context} shown={listed} {route} {place} {folded} foldable={!viewport.compact} onfold={foldSidebar} {version} onsignout={() => session.signOut()} ondismiss={(id) => session.dismiss(id)} oncreate={session.me.user ? () => (creating = true) : undefined} onsearch={() => (searching = true)} />
    </div>
    {#if viewport.narrow && drawer}
      <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
      <div class="scrim" onclick={() => (drawer = false)}></div>
    {/if}
    <main class="screen">
      <TopBar {route} {place} onmenu={viewport.narrow ? () => (drawer = true) : undefined} />
      {#key screen}
      {#if route.kind === "namespace" && route.workflow && workflowStatistics}
        {#await screens.WorkflowStatistics() then { default: WorkflowStatistics }}<WorkflowStatistics {api} {place} namespace={route.namespace} workflow={route.workflow} graph={holds(session.me, "workflow:read", route.namespace, route.workflow)} settles={settles(session.me, route.namespace, route.workflow)} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && route.workflow && operating}
        {#await screens.Runs() then { default: Runs }}<Runs {api} {place} me={session.me} namespace={route.namespace} workflow={route.workflow} record={session.namespaces.find((n) => n.name === route.namespace)} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && route.view === "workflows" && route.workflow && route.run === undefined && (route.tab === undefined || route.tab === "graph" || route.tab === "mcp" || route.tab === "files" || route.tab === "settings") && known && holdsSomewhereIn(session.me, "workflow:read", route.namespace)}
        <!-- A workflow's page: the API answers one the
             caller cannot read as one that does not exist, and the page says no more. -->
        {#await screens.Workflow() then { default: Workflow }}<Workflow {api} {place} me={session.me} namespace={route.namespace} workflow={route.workflow} tab={route.tab} namespaces={session.namespaces} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && runs && route.run && route.against}
        {#await screens.RunDiff() then { default: RunDiff }}<RunDiff {api} {place} me={session.me} namespace={route.namespace} a={route.run} b={route.against} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && runs && route.run}
        {#await screens.Run() then { default: Run }}<Run {api} {place} me={session.me} namespace={route.namespace} id={route.run} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && runs && route.workflow}
        {#await screens.Runs() then { default: Runs }}<Runs {api} {place} me={session.me} namespace={route.namespace} workflow={route.workflow} record={session.namespaces.find((n) => n.name === route.namespace)} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && (!known || !shown.some((v) => v.view === route.view) || route.run !== undefined || route.tab === "runs")}
        <Refused />
      {:else if route.kind === "namespace" && route.view === "workflows" && route.workflow === undefined}
        {#await screens.Workflows() then { default: Workflows }}<Workflows {api} {place} me={session.me} namespace={route.namespace} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && route.view === "sharing"}
        {#await screens.Sharing() then { default: Sharing }}<Sharing {api} {place} me={session.me} namespace={route.namespace} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && route.view === "variables"}
        {#await screens.Variables() then { default: Variables }}<Variables {api} {place} me={session.me} namespace={route.namespace} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && route.view === "settings"}
        {#await screens.Settings() then { default: Settings }}<Settings {api} {place} me={session.me} namespace={route.namespace} record={session.namespaces.find((n) => n.name === route.namespace)} changed={() => session.read()} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespace" && route.view === "statistics"}
        {#await screens.Statistics() then { default: Statistics }}<Statistics {api} {place} namespace={route.namespace} record={session.namespaces.find((n) => n.name === route.namespace)} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "landing"}
        {#await screens.Home() then { default: Home }}<Home {api} {place} me={session.me} namespaces={session.namespaces} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "account"}
        {#await screens.Account() then { default: Account }}<Account {api} {place} me={session.me} tab={route.tab} {passkeys} changed={() => session.read()} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "users" && route.tab === "audit" && session.me.admin}
        {#await screens.Audit() then { default: Audit }}<Audit {api} {place} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "users" && route.tab === "policy" && session.me.admin}
        {#await screens.Policy() then { default: Policy }}<Policy {api} {place} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "users" && route.tab === undefined && session.me.admin}
        {#await screens.Users() then { default: Users }}<Users {api} {place} me={session.me} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "groups" && session.me.admin}
        {#await screens.Groups() then { default: Groups }}<Groups {api} {place} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "namespaces" && session.me.admin}
        {#await screens.Namespaces() then { default: Namespaces }}<Namespaces {api} {place} changed={() => session.read()} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "runners" && route.tab === undefined && session.me.admin}
        {#await screens.Fleet() then { default: Fleet }}<Fleet {api} {place} namespaces={session.namespaces} {version} />{:catch}<Unloaded />{/await}
      {:else if route.kind === "runners" && route.tab === "statistics" && session.me.admin}
        {#await screens.PoolStatistics() then { default: PoolStatistics }}<PoolStatistics {api} {place} />{:catch}<Unloaded />{/await}
      {:else}
        <Refused />
      {/if}
      {/key}
    </main>
  </div>
  <KeyLine {keys} />
  <NewNamespace {api} bind:open={creating} {created} />
  <Palette {api} {place} bind:open={searching} {entries} reads={followed} />
{/if}

<style>
  .frame {
    display: grid;
    grid-template-columns: var(--sidebar-width) minmax(0, 1fr);
    grid-template-areas: "side screen";
    min-height: 100dvh;
  }

  .frame.folded {
    grid-template-columns: var(--sidebar-collapsed) minmax(0, 1fr);
  }

  /* The sidebar stays in the window as the page scrolls under it, and scrolls on its own where the
     window is shorter than its list. */
  .side {
    position: sticky;
    top: 0;
    grid-area: side;
    height: 100dvh;
  }

  .frame > .screen {
    grid-area: screen;
  }

  .side.blank {
    border-right: var(--border-hairline) solid var(--line);
    background: var(--surface);
  }

  /* A screen's content is at most 1280px wide, centred in the room the sidebar leaves: wider, a
     table's line runs further than the eye follows from a name to its last cell, and a wide window
     shows the same page with more margin. */
  .screen {
    justify-self: center;
    width: 100%;
    max-width: calc(1280px + 2 * (var(--padding-page) + 6px));
    min-width: 0;
    padding: calc(var(--padding-page) + 6px) calc(var(--padding-page) + 6px) calc(var(--padding-page) + 12px);
  }

  /* A screen whose last block takes the height the window has left, a workflow's graph beside its
     step, is laid out as a column, from 1100px where the two sit side by side. */
  @media (min-width: 1100px) {
    .screen:has(> :global(.fills)) {
      display: flex;
      flex-direction: column;
    }

    .screen:has(> :global(.fills)) > :global(*) {
      flex-shrink: 0;
    }
  }

  .alone {
    max-width: 480px;
    margin: 18vh auto 0;
  }

  /* Under 760px the screen takes the window's whole width, and the sidebar opens over it as a drawer. */
  .frame.narrow {
    grid-template-columns: minmax(0, 1fr);
    grid-template-areas: "screen";
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
