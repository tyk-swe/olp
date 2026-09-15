<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import SettingsPage from '../SettingsPage.svelte';

  let { client }: { client: QueryClient } = $props();
</script>

<QueryClientProvider {client}><SettingsPage /></QueryClientProvider>
