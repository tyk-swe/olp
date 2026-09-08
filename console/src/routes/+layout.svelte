<script lang="ts">
  import { QueryClient, QueryClientProvider } from '@tanstack/svelte-query';
  import { onDestroy, onMount } from 'svelte';
  import { authLifecycle } from '$lib/features/access/session/lifecycle';
  import { retryQuery } from '$lib/api/http';
  import '../app.css';

  let { children } = $props();
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: retryQuery,
        retryDelay: 1_000,
        staleTime: 5_000,
        queryKeyHashFn: (queryKey) => authLifecycle.queryKeyHash(queryKey)
      }
    }
  });
  const detachQueryClient = authLifecycle.attachQueryClient(queryClient);

  onMount(() => authLifecycle.connectTabs());

  onDestroy(() => {
    authLifecycle.abortAuthenticationWork();
    detachQueryClient();
    queryClient.clear();
  });
</script>

<QueryClientProvider client={queryClient}>
  {@render children()}
</QueryClientProvider>
