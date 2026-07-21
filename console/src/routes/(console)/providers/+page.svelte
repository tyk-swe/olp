<script lang="ts">
  import { getContext } from 'svelte';
  import ProviderList from '$lib/features/gateway/ProviderList.svelte';
  import {
    providerPaginationContext,
    type ProviderPagination
  } from '$lib/features/gateway/providerPagination';

  const pagination = getContext<ProviderPagination>(providerPaginationContext);
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

<ProviderList
  providerCursor={pagination.cursor}
  providerHistory={pagination.history}
  onProviderCursorChange={pagination.setCursor}
  onProviderHistoryChange={pagination.setHistory}
/>
