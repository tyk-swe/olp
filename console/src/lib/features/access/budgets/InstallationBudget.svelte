<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import AggregateBudgetPanel from './AggregateBudgetPanel.svelte';

  const access = useRole();
</script>

<svelte:head><title>Spending limit · OpenLLMProxy</title></svelte:head>

{#if access.globalScope}
  <AggregateBudgetPanel
    editable={access.allows('PUT /api/v1/budgets/installation')}
  />
{:else}
  <EmptyState title="Installation limit is set by an installation owner">
    Limits for your organizations and projects are on each project page.
  </EmptyState>
{/if}
