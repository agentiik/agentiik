<script lang="ts">
  import Pane from "../components/Pane.svelte";

  // What an address the caller may not open draws: the API's own words, the same whether what it names
  // does not exist or exists and is not theirs, so that the console is no way of learning which
  // namespaces and workflows exist. "No matching grant means refusal: the same 404 whether the
  // workflow is absent or merely invisible."
</script>

<div class="refused">
  <Pane title="Not found">
    <p>This page does not exist, or is not shared with you.</p>
  </Pane>
</div>

<style>
  .refused {
    max-width: 560px;
    margin: 12vh auto 0;
  }

  p {
    margin: 0;
    font-size: var(--type-navigation-size);
  }
</style>
