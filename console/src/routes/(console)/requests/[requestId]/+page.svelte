<script lang="ts">
  import { page } from '$app/state';
  import { getContext } from 'svelte';
  import RequestsPage from '$lib/features/operations/RequestsPage.svelte';
  import {
    requestListStateContext,
    type RequestListState
  } from '$lib/features/operations/requestListState';

  const listState = getContext<RequestListState>(requestListStateContext);
</script>

<RequestsPage requestId={page.params.requestId} {listState} />
