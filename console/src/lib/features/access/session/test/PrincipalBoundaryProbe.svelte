<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ConsoleLayout from '../../../../../routes/(console)/+layout.svelte';
  import PrincipalDraft from './PrincipalDraft.svelte';
  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}>
  <ConsoleLayout><PrincipalDraft /></ConsoleLayout>
</QueryClientProvider>
