<script lang="ts">
  import { page } from '$app/state';
  import InvestigationDetailPage from '$lib/features/agents/InvestigationDetailPage.svelte';
</script>

{#key page.params.investigationId}
  <InvestigationDetailPage
    investigationID={page.params.investigationId ?? ''}
  />
{/key}
