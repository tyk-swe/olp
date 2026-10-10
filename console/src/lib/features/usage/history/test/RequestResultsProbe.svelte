<script lang="ts">
  import {
    QueryClientProvider,
    createQuery,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { CursorPage } from '$lib/api/http';
  import type { RequestSummary } from '$lib/features/usage/history/api';
  import type { RequestListState } from '$lib/features/usage/history/requestListState';
  import RequestResults from '../RequestResults.svelte';

  let {
    client,
    items,
    state = 'ready',
    listState,
    applyFilters,
    resetFilters,
    problem,
    queryProblem
  }: {
    client: QueryClient;
    items: RequestSummary[];
    state?: 'ready' | 'pending' | 'error' | 'placeholder';
    listState: RequestListState;
    applyFilters: (event: SubmitEvent) => void;
    resetFilters: () => void;
    problem: string | null;
    queryProblem: string | null;
  } = $props();

  const requests = createQuery(
    () => ({
      queryKey: ['probe-requests', state],
      queryFn: async (): Promise<CursorPage<RequestSummary>> => {
        if (state === 'pending' || state === 'placeholder') {
          await new Promise(() => {});
        }
        if (state === 'error') {
          throw new Error('probe failure');
        }
        return { items, nextCursor: null };
      },
      retry: false,
      placeholderData:
        state === 'placeholder' ? { items, nextCursor: null } : undefined
    }),
    () => client
  );
</script>

<QueryClientProvider {client}>
  <RequestResults
    {requests}
    bind:listState
    {applyFilters}
    {resetFilters}
    {problem}
    {queryProblem}
  />
</QueryClientProvider>
