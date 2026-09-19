<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ModelsPage from '../ModelsPage.svelte';

  let { client }: { client: QueryClient } = $props();
  let visible = $state(true);
</script>

<QueryClientProvider {client}>
  <button type="button" onclick={() => (visible = !visible)}>
    {visible ? 'Open detail' : 'Return to list'}
  </button>
  {#if visible}<ModelsPage />{/if}
</QueryClientProvider>
