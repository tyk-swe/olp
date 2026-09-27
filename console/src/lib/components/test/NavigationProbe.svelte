<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import Navigation from '../Navigation.svelte';
  import type { Grant } from '$lib/features/access/session/authorization';

  let { client, grant }: { client: QueryClient; grant: Grant } = $props();
</script>

<QueryClientProvider {client}><Navigation {grant} /></QueryClientProvider>
