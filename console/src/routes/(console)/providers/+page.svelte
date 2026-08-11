<script lang="ts">
  import ProviderList from '$lib/features/gateway/providers/ProviderList.svelte';
  import { getProviderPagination } from '$lib/features/gateway/providers/providerPagination';

  let pagination = getProviderPagination();
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

<ProviderList bind:listState={pagination} />
