<script lang="ts">
  import Pane from "./Pane.svelte";
  import Problem from "./Problem.svelte";

  // What a screen whose chunk could not be fetched draws: the network went, or the console was
  // released again while the page was open and the chunk this page names is no longer served, which a
  // reload of the page answers in both cases, with the release the API now serves.
  const explained = {
    what: "Could not load this screen.",
    why: "Agentiik is not reachable, or was updated since this page was opened.",
    next: "Reload the page.",
    detail: "",
    transient: true,
  };
</script>

<div class="unloaded">
  <Pane title="Not loaded">
    <Problem {explained} onretry={() => location.reload()} />
  </Pane>
</div>

<style>
  .unloaded {
    max-width: 560px;
    margin: 12vh auto 0;
  }
</style>
