<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import AccountEnrollment from '../AccountEnrollment.svelte';
  let {
    client,
    onComplete
  }: { client: QueryClient; onComplete: (id: string) => void } = $props();
</script>

<QueryClientProvider {client}>
  <AccountEnrollment
    providerId="provider"
    allowed={true}
    {onComplete}
    onBusyChange={() => {}}
  />
</QueryClientProvider>
