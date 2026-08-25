<script lang="ts">
  // Test-only component: `useRole` registers a `$effect`, which only runs once
  // a real component is mounted, so the helper has to be exercised the way
  // every gated component uses it.
  import type { Capability } from '../authorization';
  import { useRole } from '../useRole.svelte';

  let { capability }: { capability: Capability } = $props();
  const access = useRole();
</script>

<p data-testid="role">{access.role ?? 'none'}</p>
<p data-testid="email">{access.user?.email ?? 'none'}</p>
<p data-testid="capability">{access.can(capability) ? 'granted' : 'denied'}</p>
