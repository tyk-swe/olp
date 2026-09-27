<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import PricingRevisionsPanel from '../PricingRevisionsPanel.svelte';

  let {
    client,
    blocked = false,
    onBusyChange,
    onFeedback
  }: {
    client: QueryClient;
    blocked?: boolean;
    onBusyChange: (busy: boolean) => void;
    onFeedback: (feedback: { status: string; error: string }) => void;
  } = $props();
</script>

<QueryClientProvider {client}>
  <PricingRevisionsPanel {blocked} {onBusyChange} {onFeedback} />
</QueryClientProvider>
