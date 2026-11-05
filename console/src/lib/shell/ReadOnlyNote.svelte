<script lang="ts">
  import type { Snippet } from 'svelte';

  // One note for every panel that a role can read but not change. `role` is a
  // prop because a couple of these explain a disabled control rather than a
  // permission, and those are not announced as notes.
  let {
    children,
    role = 'note'
  }: { children: Snippet; role?: string | undefined } = $props();
</script>

<p class="read-only-note" {role}>{@render children()}</p>
