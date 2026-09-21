<script lang="ts">
  import { untrack } from 'svelte';
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import MediaJobsPage from '../MediaJobsPage.svelte';
  import { mediaJobList, type MediaJobListState } from '../mediaJobListState';

  let {
    client,
    jobId = '',
    initialState
  }: {
    client: QueryClient;
    jobId?: string;
    initialState?: MediaJobListState;
  } = $props();
  let listState = $state(untrack(() => initialState ?? mediaJobList.empty()));
</script>

<QueryClientProvider {client}>
  <MediaJobsPage {jobId} bind:listState />
</QueryClientProvider>
