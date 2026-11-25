<script lang="ts">
  import { QueryClient, QueryClientProvider } from '@tanstack/svelte-query';
  import { onDestroy, onMount } from 'svelte';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import { retryQuery } from '#lib/management/http.js';
  import '../app.css';
  import Toaster from '#lib/components/Toaster.svelte';
  import MFAConfirmationHost from '#lib/features/identity/mfa/MFAConfirmationHost.svelte';

  let { children } = $props();
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: retryQuery,
        retryDelay: 1_000,
        staleTime: 30_000,
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
  <MFAConfirmationHost />
  <Toaster />
</QueryClientProvider>
