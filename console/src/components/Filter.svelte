<script lang="ts">
  import { useKeys } from "../lib/keys.svelte";
  import type { Place } from "../lib/place.svelte";
  import Icon from "./Icon.svelte";

  // The Filter field at the head of a screen whose list the API answers whole: what is typed narrows
  // the list as lib/palette's filtered matches it, and is kept in the address as q, so that a narrowed
  // list is a link. / puts the cursor here, as it opens agk console's filter, and esc leaves it.
  let { place, label }: { place: Place; label: string } = $props();

  let field = $state<HTMLInputElement | undefined>();
  const typed = $derived(place.query.get("q") ?? "");

  function input(e: Event & { currentTarget: HTMLInputElement }) {
    const q = new URLSearchParams(place.query);
    const value = e.currentTarget.value;
    if (value.trim() === "") q.delete("q");
    else q.set("q", value);
    place.narrow(q);
  }

  function keydown(e: KeyboardEvent & { currentTarget: HTMLInputElement }) {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      e.currentTarget.blur();
    }
  }

  useKeys(() => [{ keys: ["/"], effect: "Filter", does: () => field?.focus() }]);
</script>

<label class="filter">
  <Icon name="control-filter" size={14} />
  <input bind:this={field} type="search" value={typed} oninput={input} onkeydown={keydown} placeholder="Filter" aria-label={label} autocomplete="off" spellcheck="false" />
  <kbd aria-hidden="true">/</kbd>
</label>

<style>
  .filter {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    width: 240px;
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--surface);
    color: var(--muted);
    cursor: text;
  }

  .filter:focus-within {
    border-color: var(--accent);
  }

  input {
    flex: 1;
    min-width: 0;
    height: 100%;
    padding: 0;
    border: none;
    background: none;
    color: var(--text);
    font: inherit;
    font-size: var(--type-control-size);
    outline: none;
  }

  input::placeholder {
    color: var(--faint);
  }

  input::-webkit-search-cancel-button {
    display: none;
  }

  /* As the key line draws a key, quieter, and gone once something is typed or the field is in use. */
  kbd {
    min-width: 18px;
    padding: 0 calc(var(--unit) * 2);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-chip);
    color: var(--faint);
    font-family: var(--type-identifier-font);
    font-size: var(--type-identifier-size-min);
    line-height: 18px;
    text-align: center;
  }

  .filter:focus-within kbd {
    visibility: hidden;
  }

  @media (max-width: 759px) {
    .filter {
      flex: 1 1 auto;
      width: auto;
      min-width: 0;
    }
  }
</style>
