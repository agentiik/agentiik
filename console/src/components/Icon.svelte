<script lang="ts">
  import { icons } from "../lib/icons";

  // One icon of the design system, by the name its file has in agentiik/design: control-replay,
  // state-failed, port-rejected. It is drawn in the colour of the text around it and read by nobody,
  // since the word beside it says what it means.
  let { name, size = 16 }: { name: string; size?: number } = $props();
</script>

<span class="icon" style:width="{size}px" style:height="{size}px" aria-hidden="true">{@html icons[name] ?? ""}</span>

<style>
  .icon {
    display: inline-flex;
    flex: none;
  }

  .icon :global(svg) {
    width: 100%;
    height: 100%;
  }
</style>
