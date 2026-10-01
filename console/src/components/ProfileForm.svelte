<script lang="ts">
  import { explain, refused, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { untrack } from "svelte";
  import type { API, Me } from "../api/client";
  import { changed, longest, localTime, photoBytes, photoOf, photoTypes, profileOf, removePhoto, save, setPhoto, zones, type Field, type Profile } from "../lib/profile";
  import { sentence } from "../lib/signin";
  import Avatar from "./Avatar.svelte";
  import Icon from "./Icon.svelte";
  import Notice from "./Notice.svelte";
  import Pane from "./Pane.svelte";

  // The caller's profile and photo, which they alone write: PATCH /api/v1/me sends only what the form
  // changed, and the photo is sent as the file chosen, which the API stores again as its pixels alone.
  // reread is the session read again, so that the name and the face in the top bar follow at once.
  let { api, me, reread }: { api: API; me: Me; reread: () => Promise<void> } = $props();

  const user = $derived(me.user);
  let form = $state<Profile>(untrack(() => profileOf(me.user ?? ({ display_name: "" } as never))));
  const before = $derived(user ? profileOf(user) : form);
  const edits = $derived(changed(before, form));
  const dirty = $derived(Object.keys(edits).length > 0);

  let working = $state(false);
  let said = $state("");
  let problem = $state<Explained | null>(null);

  async function act(failed: string, work: () => Promise<string>) {
    if (working) return;
    working = true;
    said = "";
    problem = null;
    try {
      said = await work();
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  function submit(e: SubmitEvent) {
    e.preventDefault();
    if (!dirty) return;
    return act("save your profile", async () => {
      const answer = await save(api, edits);
      if (answer.user) form = profileOf(answer.user);
      await reread();
      return "Your profile is saved.";
    });
  }

  const labels: Record<Field, string> = {
    display_name: "Display name",
    given_name: "Given name",
    family_name: "Family name",
    title: "Title",
    location: "Location",
    timezone: "Time zone",
    bio: "Bio",
  };
  const hints: Partial<Record<Field, string>> = {
    display_name: "the name people read beside your login",
    timezone: "as the IANA database names it, such as Europe/Paris",
  };
  const placeholders: Partial<Record<Field, string>> = { title: "Technical lead", location: "Lyon, France", timezone: "Europe/Paris" };
  const known = zones();
  const there = $derived(localTime(form.timezone.trim(), Date.now()));

  // The photo: a file chosen is checked for what the API takes before it is sent.
  let picker = $state<HTMLInputElement | undefined>();
  const photo = $derived(photoOf(me));

  function chosen(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    input.value = "";
    if (!file) return;
    if (!photoTypes.includes(file.type)) {
      problem = refused("set your photo", "A photo must be a PNG or a JPEG file.");
      return;
    }
    if (file.size > photoBytes) {
      problem = refused("set your photo", "A photo must be 1 MiB or smaller.");
      return;
    }
    return act("set your photo", async () => {
      await setPhoto(api, file);
      await reread();
      return "Your photo is set. You and the installation's administrators can see it.";
    });
  }

  function remove() {
    return act("remove your photo", async () => {
      await removePhoto(api);
      await reread();
      return "Your photo is removed. Your initial is shown instead.";
    });
  }
</script>

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

{#if !user}
  <Pane title="Profile">
    <p class="muted">A service account has no profile: it is identified by its namespace and its name only.</p>
  </Pane>
{:else}
  <div class="columns">
    <Pane title="Profile" aside="shown to the people you work with">
      <form onsubmit={submit} aria-label="Your profile">
        <div class="pair">
          {#each ["given_name", "family_name"] as const as f (f)}
            <label>
              <span>{labels[f]}</span>
              <input bind:value={form[f]} maxlength={longest[f]} autocomplete={f === "given_name" ? "given-name" : "family-name"} />
            </label>
          {/each}
        </div>
        {#each ["display_name", "title", "location", "timezone"] as const as f (f)}
          <label>
            <span>{labels[f]}{#if hints[f]}<span class="faint hint">{hints[f]}</span>{/if}</span>
            <input bind:value={form[f]} maxlength={longest[f]} placeholder={placeholders[f] ?? ""} required={f === "display_name"} list={f === "timezone" ? "profile-zones" : undefined} autocomplete={f === "display_name" ? "name" : "off"} />
            {#if f === "timezone" && there}<span class="faint">{there} there now</span>{/if}
          </label>
        {/each}
        <datalist id="profile-zones">{#each known as z (z)}<option value={z}></option>{/each}</datalist>
        <label>
          <span>{labels.bio}<span class="faint hint">{form.bio.length} of {longest.bio}</span></span>
          <textarea bind:value={form.bio} maxlength={longest.bio} rows="3"></textarea>
        </label>
        <p class="buttons">
          <button class="control primary" disabled={working || !dirty}>Save the profile</button>
          {#if dirty}<button class="control" type="button" onclick={() => user && (form = profileOf(user))}>Undo the changes</button>{/if}
        </p>
      </form>
    </Pane>

    <Pane title="Photo">
      <div class="photo">
        <Avatar name={user.display_name} src={photo} size={120} />
        <div class="acts">
          <input bind:this={picker} class="unseen" type="file" accept={photoTypes.join(",")} onchange={chosen} aria-label="A photo, a PNG or a JPEG" />
          <button class="control" disabled={working} onclick={() => picker?.click()}><Icon name="control-edit" size={14} />{photo ? "Change the photo" : "Choose a photo"}</button>
          {#if photo}<button class="control" disabled={working} onclick={remove}><Icon name="control-remove" size={14} />Remove it</button>{/if}
        </div>
        <p class="faint">A PNG or JPEG file, 1 MiB at most. It is resized to 512 by 512 pixels at most, and its metadata is removed. Shown to you and to the administrators.</p>
      </div>
    </Pane>
  </div>
{/if}

<style>
  .columns {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(280px, 1fr);
    gap: calc(var(--unit) * 8);
    align-items: start;
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 6);
    max-width: 640px;
  }

  .pair {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: calc(var(--unit) * 6);
  }

  label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  label > span:first-child {
    color: var(--muted);
    font-weight: 500;
  }

  textarea {
    resize: vertical;
  }

  .hint {
    margin-left: calc(var(--unit) * 3);
    font-weight: 400;
  }

  .buttons {
    display: flex;
    gap: calc(var(--unit) * 3);
    margin: 0;
  }

  .photo {
    display: grid;
    justify-items: start;
    gap: calc(var(--unit) * 6);
  }

  .acts {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3);
  }

  .photo p {
    margin: 0;
    font-size: var(--type-control-size);
  }

  @media (max-width: 1099px) {
    .columns {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  @media (max-width: 759px) {
    .pair {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
