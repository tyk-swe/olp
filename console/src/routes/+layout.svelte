<script lang="ts">
  import { QueryClient, QueryClientProvider } from '@tanstack/svelte-query';
  import '../app.css';

  let { children } = $props();
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: 1,
        staleTime: 5_000
      }
    }
  });
</script>

<QueryClientProvider client={queryClient}>
  {@render children()}
</QueryClientProvider>
