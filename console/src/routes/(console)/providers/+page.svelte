<script lang="ts">
  import { getContext } from 'svelte';
  import type { CursorHistory } from '$lib/api/pagination';
  import ProviderList from '$lib/features/gateway/ProviderList.svelte';
  import { providerPaginationContext } from '$lib/features/gateway/providerPagination';

  let pagination = getContext<CursorHistory>(providerPaginationContext);
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

<ProviderList bind:listState={pagination} />
