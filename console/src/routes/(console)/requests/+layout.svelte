<script lang="ts">
  import { setContext } from 'svelte';
  import {
    requestListStateContext,
    type RequestListState
  } from '$lib/features/operations/requestListState';

  let { children } = $props();
  const listState = $state<RequestListState>({
    route: '',
    providerId: '',
    model: '',
    apiKeyId: '',
    operation: '',
    statusCode: '',
    errorClass: '',
    startedAfter: '',
    startedBefore: '',
    applied: { limit: 25 },
    cursors: []
  });

  setContext(requestListStateContext, listState);
</script>

{@render children()}
