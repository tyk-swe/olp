<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderDetail from '../ProviderDetail.svelte';
  import ProviderWizard from '../ProviderWizard.svelte';

  let { client, providerId }: { client: QueryClient; providerId?: string } =
    $props();
</script>

<QueryClientProvider {client}>
  {#if providerId}<ProviderDetail {providerId} />{:else}<ProviderWizard />{/if}
</QueryClientProvider>
