<script lang="ts">
  import ProviderList from '$lib/features/gateway/providers/ProviderList.svelte';
  import { providerList } from '$lib/features/gateway/providers/providerPagination';

  let pagination = providerList.get();
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

<ProviderList bind:listState={pagination} />
