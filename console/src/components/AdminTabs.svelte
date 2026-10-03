<script lang="ts">
  import type { Snippet } from "svelte";
  import type { Place } from "../lib/place.svelte";
  import PageHeader from "./PageHeader.svelte";

  // The head of what an administrator manages of the installation: the users, with the sign-in
  // policy they are held to as a tab beside them, the groups they are put in, or the namespaces with
  // their owners and quotas, and the audit log of what was done to all of them, each an entry of the
  // sidebar of its own.
  let { place, current, actions }: { place: Place; current: "users" | "policy" | "groups" | "namespaces" | "audit"; actions?: Snippet } = $props();

  const heads = {
    users: { title: "Users", icon: "control-users" },
    policy: { title: "Users", icon: "control-users" },
    groups: { title: "Groups", icon: "control-groups" },
    namespaces: { title: "Namespaces", icon: "control-namespaces" },
    audit: { title: "Audit log", icon: "control-history" },
  };

  const tabs = $derived(
    current === "users" || current === "policy"
      ? [
          { label: "Users", icon: "control-users", to: { kind: "users" as const }, current: current === "users" },
          { label: "Sign-in policy", icon: "control-passkey", to: { kind: "users" as const, tab: "policy" }, current: current === "policy" },
        ]
      : undefined,
  );
</script>

<PageHeader title={heads[current].title} icon={heads[current].icon} {place} {tabs} {actions} />
