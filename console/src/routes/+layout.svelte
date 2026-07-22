<script lang="ts">
  import { QueryClient, QueryClientProvider } from '@tanstack/svelte-query';
  import { onDestroy } from 'svelte';
  import { authLifecycle } from '$lib/auth/lifecycle';
  import '../app.css';

  let { children } = $props();
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: 1,
        staleTime: 5_000,
        queryKeyHashFn: (queryKey) => authLifecycle.queryKeyHash(queryKey)
      }
    }
  });
  const detachQueryClient = authLifecycle.attachQueryClient(queryClient);

  onDestroy(() => {
    authLifecycle.abortAuthenticationWork();
    detachQueryClient();
    queryClient.clear();
  });
</script>

<QueryClientProvider client={queryClient}>
  {@render children()}
</QueryClientProvider>
