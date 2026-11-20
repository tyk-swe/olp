<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import AggregateBudgetPanel from './AggregateBudgetPanel.svelte';

  const access = useRole();
</script>

<PageTitle title="Spending limit" />

{#if access.globalScope}
  <AggregateBudgetPanel
    editable={access.allows('PUT /api/v1/budgets/installation')}
  />
{:else}
  <EmptyState title="Installation limit is set by an installation owner">
    Limits for your organizations and projects are on each project page.
  </EmptyState>
{/if}
