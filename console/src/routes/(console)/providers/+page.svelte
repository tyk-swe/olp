<script lang="ts">
  import ProviderList from '$lib/features/providers/ProviderList.svelte';
  import { providerList } from '$lib/features/providers/providerPagination';

  let pagination = providerList.get();
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

<ProviderList bind:listState={pagination} />
