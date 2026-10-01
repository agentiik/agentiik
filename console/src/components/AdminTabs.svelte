<script lang="ts">
  import type { Snippet } from "svelte";
  import type { Place } from "../lib/place.svelte";
  import PageHeader from "./PageHeader.svelte";

  // The head of what an administrator manages of the installation: the users, the groups they are put
  // in, or the namespaces with their owners and quotas, each an entry of the sidebar of its own.
  let { place, current, actions }: { place: Place; current: "users" | "groups" | "namespaces"; actions?: Snippet } = $props();

  const heads = {
    users: { title: "Users", icon: "control-users" },
    groups: { title: "Groups", icon: "control-groups" },
    namespaces: { title: "Namespaces", icon: "control-namespaces" },
  };
</script>

<PageHeader title={heads[current].title} icon={heads[current].icon} {place} {actions} />
