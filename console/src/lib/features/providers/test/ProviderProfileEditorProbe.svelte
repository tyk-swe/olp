<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderProfileEditor from '../ProviderProfileEditor.svelte';
  import type { ProviderEditValues } from '../providerEditor';

  let { client, values }: { client: QueryClient; values: ProviderEditValues } =
    $props();
</script>

<QueryClientProvider {client}>
  <ProviderProfileEditor {values} idPrefix="probe" onChange={() => {}} />
</QueryClientProvider>
