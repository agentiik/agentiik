<script lang="ts">
  import { onMount } from "svelte";
  import type { API } from "./api/client";
  import KeyLine, { type Key } from "./components/KeyLine.svelte";
  import Pane from "./components/Pane.svelte";
  import TopBar from "./components/TopBar.svelte";
  import { holds, holdsSomewhereIn, home, inNamespace } from "./lib/permissions";
  import type { Place } from "./lib/place.svelte";
  import type { View } from "./lib/route";
  import type { Session } from "./lib/session.svelte";
  import Account from "./views/Account.svelte";
  import PoolStatistics from "./views/PoolStatistics.svelte";
  import Refused from "./views/Refused.svelte";
  import Run from "./views/Run.svelte";
  import RunDiff from "./views/RunDiff.svelte";
  import Runs from "./views/Runs.svelte";
  import SignIn, { type Passkeys } from "./views/SignIn.svelte";
  import Statistics from "./views/Statistics.svelte";
  import Users from "./views/Users.svelte";
  import Workflow from "./views/Workflow.svelte";
  import WorkflowStatistics from "./views/WorkflowStatistics.svelte";

  // The console: who it is signed in as, the top bar, the screen the address names, and the key line.
  let { api, session, place, version, passkeys }: { api: API; session: Session; place: Place; version: string; passkeys: Passkeys } = $props();

  onMount(() => {
    session.read();
  });

  const route = $derived(place.route);
  const namespace = $derived(route.kind === "namespace" ? route.namespace : undefined);

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
  const built = new Set<View>(["runs", "statistics"]);

  const known = $derived(namespace !== undefined && session.namespaces.some((n) => n.name === namespace));
  const shown = $derived(namespace && known ? all.filter((v) => built.has(v.view) && v.shows(namespace)) : []);

  // A workflow's own statistics, the one page of a workflow built so far, open to whoever reads runs
  // somewhere in its namespace: the series answer a workflow the caller cannot read as one that does
  // not exist, so the page says no more than the API does.
  const workflowStatistics = $derived(
    route.kind === "namespace" && route.view === "workflows" && route.workflow !== undefined && route.tab === "statistics" && known && !!session.me && holdsSomewhereIn(session.me, "run:read", route.namespace),
  );

  // The address with nothing after the console's root opens the caller's own namespace.
  $effect(() => {
    if (session.standing === "signed-in" && session.me && route.kind === "landing") {
      const ns = home(session.me, session.namespaces);
      if (ns) {
        place.go({ kind: "namespace", namespace: ns, view: "runs" }, true);
      }
    }
  });

  // The keys of the view, named by their effect; none yet beyond what the browser gives.
  const keys: Key[] = [];
</script>

{#if session.standing === "reading"}
  <p class="reading" role="status">Reading who you are.</p>
{:else if session.standing === "signed-out"}
  <SignIn {api} {session} {passkeys} />
{:else if session.standing === "enrol-only"}
  <main class="alone">
    <Pane title="Enrol a passkey">
      <p>This session may enrol a passkey, and nothing else until one is.</p>
      <p><a class="control primary" href="auth/enrol">Enrol a passkey</a></p>
    </Pane>
  </main>
{:else if session.standing === "unreachable"}
  <main class="alone">
    <Pane title="Not answering">
      <p>The installation did not answer: {session.said}</p>
      <p><button class="control" onclick={() => session.read()}>Ask again</button></p>
    </Pane>
  </main>
{:else if session.me}
  <div class="frame">
    <TopBar
      me={session.me}
      namespaces={session.namespaces}
      {namespace}
      view={route.kind === "namespace" ? route.view : undefined}
      {shown}
      answering={session.answering}
      {place}
      onsignout={() => session.signOut()}
      ondismiss={(id) => session.dismiss(id)}
    />
    <main class="screen">
      {#if route.kind === "namespace" && route.workflow && workflowStatistics}
        <WorkflowStatistics {api} {place} namespace={route.namespace} workflow={route.workflow} graph={holds(session.me, "workflow:read", route.namespace, route.workflow)} />
      {:else if route.kind === "namespace" && route.view === "workflows" && route.workflow && (route.tab === undefined || route.tab === "graph") && known && holdsSomewhereIn(session.me, "workflow:read", route.namespace)}
        <!-- A workflow's page, the one view under workflows built so far: the API answers one the
             caller cannot read as one that does not exist, and the page says no more. -->
        <Workflow {api} {place} me={session.me} namespace={route.namespace} workflow={route.workflow} />
      {:else if route.kind === "namespace" && (!known || !shown.some((v) => v.view === route.view))}
        <Refused />
      {:else if route.kind === "namespace" && route.view === "runs" && route.run && route.against}
        <RunDiff {api} {place} me={session.me} namespace={route.namespace} a={route.run} b={route.against} />
      {:else if route.kind === "namespace" && route.view === "runs" && route.run}
        <Run {api} {place} me={session.me} namespace={route.namespace} id={route.run} />
      {:else if route.kind === "namespace" && route.view === "runs"}
        <Runs {api} {place} me={session.me} namespace={route.namespace} record={session.namespaces.find((n) => n.name === route.namespace)} />
      {:else if route.kind === "namespace" && route.view === "statistics"}
        <Statistics {api} {place} namespace={route.namespace} record={session.namespaces.find((n) => n.name === route.namespace)} />
      {:else if route.kind === "account"}
        <Account {api} {place} me={session.me} tab={route.tab} {passkeys} changed={() => session.read()} />
      {:else if route.kind === "users" && session.me.admin}
        <Users {api} me={session.me} />
      {:else if route.kind === "runners" && route.tab === "statistics" && session.me.admin}
        <PoolStatistics {api} {place} />
      {:else}
        <Refused />
      {/if}
    </main>
    <KeyLine {keys} {version} />
  </div>
{/if}

<style>
  .frame {
    display: grid;
    grid-template-rows: var(--bar-top) 1fr var(--bar-keyLine);
    height: 100%;
  }

  .screen {
    min-height: 0;
    padding: calc(var(--padding-page) + 6px) var(--padding-page) var(--padding-page);
    overflow: auto;
  }

  .alone {
    max-width: 480px;
    margin: 18vh auto 0;
  }

  .alone p {
    margin: 0 0 calc(var(--unit) * 6);
    font-size: var(--type-navigation-size);
  }

  .alone p:last-child {
    margin-bottom: 0;
  }

  .reading {
    margin: 18vh auto 0;
    color: var(--muted);
    text-align: center;
  }
</style>
