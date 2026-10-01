<script lang="ts">
  import { explain, refused, type Explained } from "../lib/problem";
  import type { Place } from "../lib/place.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import { refusal, type API, type Me, type Namespace } from "../api/client";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import Notice from "../components/Notice.svelte";
  import NamespaceMark from "../components/NamespaceMark.svelte";
  import SecretsSection from "../components/SecretsSection.svelte";
  import { holds } from "../lib/permissions";
  import { pictureOf, removePicture, setPicture } from "../lib/namespaces";
  import { photoBytes, photoTypes } from "../lib/profile";

  // A namespace's settings, in sections one under another, each a pane: General, its name and its
  // picture; Secrets; and last, removing it. The name and the picture are changed by its owner,
  // whoever holds grant:manage there, and by an administrator, as the API decides; anybody else reads
  // them. A personal namespace is named after its user's login and goes with its user, so it is
  // never offered a rename nor a removal.
  let {
    api,
    place,
    me,
    namespace,
    record,
    changed,
  }: { api: API; place: Place; me: Me; namespace: string; record: Namespace | undefined; changed: () => Promise<void> } = $props();

  const personal = $derived(record?.kind === "personal");
  const manages = $derived(me.admin || holds(me, "grant:manage", namespace));

  let working = $state(false);
  let said = $state("");
  let problem = $state<Explained | null>(null);

  async function act(failed: string, work: () => Promise<void>) {
    if (working) return;
    working = true;
    problem = null;
    said = "";
    try {
      await work();
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  // The rename: the name typed, sent as the one field PATCH changes, then the namespace opened under
  // its new name, its settings still, once the list of namespaces is read again.
  let name = $state("");
  $effect(() => {
    name = namespace;
  });

  function rename(e: SubmitEvent) {
    e.preventDefault();
    const to = name.trim();
    if (to === namespace) return;
    return act(`rename ${namespace}`, async () => {
      const { data, error, response } = await api.PATCH("/api/v1/namespaces/{ns}", { params: { path: { ns: namespace } }, body: { name: to } });
      if (!data) throw refusal(response, error);
      await changed();
      said = `${namespace} renamed to ${data.name}.`;
      place.go({ kind: "namespace", namespace: data.name, view: "settings" });
    });
  }

  // The picture: a file chosen is checked for what the API takes before it is sent.
  let picker = $state<HTMLInputElement | undefined>();

  function chosen(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    input.value = "";
    if (!file) return;
    if (!photoTypes.includes(file.type)) {
      problem = refused("set the picture", "A picture must be a PNG or a JPEG file.");
      return;
    }
    if (file.size > photoBytes) {
      problem = refused("set the picture", "A picture must be 1 MiB or smaller.");
      return;
    }
    return act("set the picture", async () => {
      await setPicture(api, namespace, file);
      await changed();
      said = "Picture saved.";
    });
  }

  function unset() {
    return act("remove the picture", async () => {
      await removePicture(api, namespace);
      await changed();
      said = "Picture removed.";
    });
  }

  // Removing the namespace waits on a second click, and what the API refuses it for, what the
  // namespace still holds, is said as the API says it.
  let asking = $state(false);

  function remove() {
    return act(`delete ${namespace}`, async () => {
      const answer = await api.DELETE("/api/v1/namespaces/{ns}", { params: { path: { ns: namespace } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      asking = false;
      await changed();
      place.go({ kind: "landing" });
    });
  }
</script>

<PageHeader title="Settings" icon="control-settings" {place} />

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<div class="sections">
  <Pane title="General">
    <div class="general">
      <div class="field">
        <span class="label">Name</span>
        {#if manages && !personal}
          <form class="rename" onsubmit={rename} aria-label="Rename the namespace">
            <input class="term" bind:value={name} aria-label="Name" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" spellcheck="false" autocomplete="off" />
            <button class="control" disabled={working || name.trim() === namespace}>Rename</button>
          </form>
        {:else}
          <span class="term">{namespace}</span>
        {/if}
        {#if record?.former_names?.length}
          <span class="former">
            <span class="label">Former names</span>
            <span class="term">{record.former_names.join(", ")}</span>
          </span>
        {/if}
      </div>
      <div class="field">
        <span class="label">Picture</span>
        <span class="picture">
          <NamespaceMark name={namespace} src={pictureOf(record)} size={64} />
          {#if manages}
            <input bind:this={picker} class="unseen" type="file" accept={photoTypes.join(",")} onchange={chosen} aria-label="A picture, a PNG or a JPEG" />
            <button class="control" disabled={working} onclick={() => picker?.click()}><Icon name="control-edit" size={14} />Choose a picture</button>
            {#if record?.avatar_updated_at}<button class="control" disabled={working} onclick={unset}><Icon name="control-remove" size={14} />Remove it</button>{/if}
          {/if}
        </span>
      </div>
    </div>
  </Pane>

  <SecretsSection {api} {me} {namespace} />

  {#if manages && !personal}
    <Pane title="Delete namespace" label="Delete namespace">
      <span class="confirm">
        {#if asking}
          <button class="control danger" disabled={working} onclick={remove}><Icon name="control-remove" size={14} />Delete</button>
          <button class="control" onclick={() => (asking = false)}>Keep</button>
        {:else}
          <button class="control danger" onclick={() => (asking = true)}><Icon name="control-remove" size={14} />Delete</button>
        {/if}
      </span>
    </Pane>
  {/if}
</div>

<style>
  .sections {
    display: grid;
    gap: calc(var(--unit) * 8);
  }

  .general {
    display: grid;
    gap: calc(var(--unit) * 6);
  }

  .field {
    display: grid;
    grid-template-columns: 120px minmax(0, 1fr);
    align-items: center;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  .label {
    color: var(--muted);
  }

  .former {
    display: contents;
  }

  .rename {
    display: flex;
    gap: calc(var(--unit) * 3);
    max-width: 420px;
  }

  .rename input {
    flex: 1;
    min-width: 0;
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .picture {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
  }

  .confirm {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }

  @media (max-width: 759px) {
    .field {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
