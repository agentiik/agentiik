<script lang="ts">
  // A person as a round face: their photo where they set one, else the first letter of their name on
  // the accent's quiet fill, drawn at the same size either way so that a photo arriving moves nothing.
  let { name, src, size = 24 }: { name: string; src?: string; size?: number } = $props();

  const initial = $derived(name.trim().charAt(0).toUpperCase() || "?");
  let failed = $state(false);
</script>

<span class="avatar" style:width="{size}px" style:height="{size}px" style:font-size="{Math.round(size * 0.46)}px" aria-hidden="true">
  {#if src && !failed}
    <img {src} alt="" width={size} height={size} onerror={() => (failed = true)} />
  {:else}
    {initial}
  {/if}
</span>

<style>
  .avatar {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    overflow: hidden;
    border: var(--border-hairline) solid var(--accentLine);
    border-radius: var(--radius-round);
    background: var(--accentDim);
    color: var(--accent);
    font-weight: 600;
    --leading: 1;
  }

  img {
    width: 100%;
    height: 100%;
    object-fit: cover;
  }
</style>
