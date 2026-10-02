<script lang="ts">
  // A namespace as a square tile: its picture where it has one, else its initial on the accent's quiet
  // fill, at the same size either way so that a picture arriving moves nothing. Round is a person's.
  let { name, src, size = 26 }: { name: string; src?: string; size?: number } = $props();

  const initial = $derived(name.trim().charAt(0).toUpperCase() || "?");
  let failed = $state(false);
  $effect(() => {
    void src;
    failed = false;
  });
</script>

<span class="mark" style:width="{size}px" style:height="{size}px" style:font-size="{Math.round(size * 0.5)}px" aria-hidden="true">
  {#if src && !failed}
    <img {src} alt="" width={size} height={size} onerror={() => (failed = true)} />
  {:else}
    {initial}
  {/if}
</span>

<style>
  .mark {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    overflow: hidden;
    border-radius: var(--radius-control);
    background: var(--accentDim);
    color: var(--accent);
    font-weight: 700;
    --leading: 1;
  }

  img {
    width: 100%;
    height: 100%;
    object-fit: cover;
  }
</style>
