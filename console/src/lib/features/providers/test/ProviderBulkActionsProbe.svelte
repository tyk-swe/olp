<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderBulkActions from '../ProviderBulkActions.svelte';
  import type { ProviderSummary } from '../api/providers';
  let {
    client,
    selected,
    onChanged
  }: {
    client: QueryClient;
    selected: ProviderSummary[];
    onChanged: () => void | Promise<void>;
  } = $props();
  export function replaceSelection(next: ProviderSummary[]) {
    selected = next;
  }
</script>

<QueryClientProvider {client}>
  <ProviderBulkActions {selected} {onChanged} />
</QueryClientProvider>
