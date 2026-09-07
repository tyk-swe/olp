<script lang="ts">
  import { page } from '$app/state';
  import ProviderDetail from '$lib/features/providers/ProviderDetail.svelte';
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

<ProviderDetail providerId={page.params.providerId ?? ''} />
