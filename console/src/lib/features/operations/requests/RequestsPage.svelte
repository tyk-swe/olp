<script lang="ts">
  import RequestTimeline from './RequestTimeline.svelte';
  import RequestResults from './RequestResults.svelte';
  import { resolve } from '$app/paths';
  import { queryKeys } from '$lib/api/queryKeys';
  import { createQuery } from '@tanstack/svelte-query';
  import { getRequest, listRequests } from '$lib/api/requests';

  import { requestList, type RequestListState } from './requestListState';

  let {
    requestId = '',
    listState = $bindable()
  }: {
    requestId?: string;
    listState: RequestListState;
  } = $props();

  const requests = createQuery(() => ({
    queryKey: queryKeys.requests.page(listState.applied, listState.cursor),
    queryFn: () =>
      listRequests({ ...listState.applied, cursor: listState.cursor }),
    placeholderData: (previous) => previous,
    enabled: !requestId
  }));

  const detail = createQuery(() => ({
    queryKey: queryKeys.requests.detail(requestId),
    queryFn: () => getRequest(requestId),
    enabled: Boolean(requestId)
  }));

  function applyFilters(event: SubmitEvent) {
    event.preventDefault();
    requestList.apply(listState);
  }

  function resetFilters() {
    requestList.clear(listState);
  }
</script>

<svelte:head><title>Requests · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">
      {requestId ? 'Request timeline' : 'Request Explorer'}
    </h1>
    <p class="page-description">
      {requestId
        ? 'Metadata-only route decisions and upstream attempts. Request and response content is never available here.'
        : 'Filter operational metadata by route, target, key, outcome, or time range—never prompt or output content.'}
    </p>
  </div>
  {#if requestId}<a class="button button-secondary" href={resolve('/requests')}
      >Back to requests</a
    >{/if}
</div>

{#if requestId}<RequestTimeline {detail} />{:else}<RequestResults
    {requests}
    bind:listState
    {applyFilters}
    {resetFilters}
  />{/if}
