<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderDetail from '../ProviderDetail.svelte';
  import ProviderWizard from '../ProviderWizard.svelte';
  import ProviderDetailPage from '../../../../routes/(console)/providers/[providerId]/+page.svelte';

  let {
    client,
    providerId,
    providerRoute = false
  }: {
    client: QueryClient;
    providerId?: string;
    providerRoute?: boolean;
  } = $props();
</script>

<QueryClientProvider {client}>
  {#if providerRoute}<ProviderDetailPage />{:else if providerId}<ProviderDetail
      {providerId}
    />{:else}<ProviderWizard />{/if}
</QueryClientProvider>
