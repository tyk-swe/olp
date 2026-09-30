<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ConsoleLayout from '../+layout.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <ConsoleLayout><p>Protected content</p></ConsoleLayout>
</QueryClientProvider>
