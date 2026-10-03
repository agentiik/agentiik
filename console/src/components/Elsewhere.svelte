<script lang="ts">
  import Icon from "./Icon.svelte";

  // The console opened at an address other than the installation's public URL, said above everything
  // it draws, on the sign-in page and on every screen, since nothing that signs in or changes anything
  // works from here: the same page at the address that works, as a link.
  let { href }: { href: string } = $props();
</script>

<div class="elsewhere" role="alert">
  <Icon name="state-failed" size={16} />
  <p>Sign-in and changes work at <a {href}>{new URL(href).origin}</a> only.</p>
</div>

<style>
  .elsewhere {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    min-height: var(--control-height);
    padding: 0 calc(var(--padding-page) + 6px);
    border-bottom: var(--border-hairline) solid var(--line);
    background: var(--surface);
    color: var(--failed);
    font-size: var(--type-control-size);
  }

  p {
    margin: 0;
    color: var(--text);
  }

  a {
    color: inherit;
    font-weight: 600;
    overflow-wrap: anywhere;
  }
</style>
