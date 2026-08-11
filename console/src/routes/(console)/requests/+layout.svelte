<script lang="ts">
  import {
    emptyRequestListState,
    setRequestListState,
    type RequestListState
  } from '$lib/features/operations/requests/requestListState';

  let { children } = $props();
  const listState = $state<RequestListState>(emptyRequestListState());

  setRequestListState(listState);
</script>

{@render children()}
