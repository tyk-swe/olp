<script lang="ts">
  import { page } from '$app/state';
  import ProviderDetail from '$lib/features/providers/ProviderDetail.svelte';
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

{#key page.params.providerId}
  <ProviderDetail providerId={page.params.providerId ?? ''} />
{/key}
