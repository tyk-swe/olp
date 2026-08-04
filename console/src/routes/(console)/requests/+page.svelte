<script lang="ts">
  import { getContext } from 'svelte';
  import RequestsPage from '$lib/features/operations/requests/RequestsPage.svelte';
  import {
    requestListStateContext,
    type RequestListState
  } from '$lib/features/operations/requests/requestListState';

  let listState = getContext<RequestListState>(requestListStateContext);
</script>

<RequestsPage bind:listState />
