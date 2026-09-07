<script lang="ts">
  import RequestTimeline from './RequestTimeline.svelte';
  import RequestResults from './RequestResults.svelte';
  import { resolve } from '$app/paths';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { queryKeys } from '$lib/api/queryKeys';
  import { createQuery } from '@tanstack/svelte-query';
  import { getRequest, listRequests } from '$lib/api/requests';

  import {
    requestFilters,
    requestProblem,
    requestSearch,
    requestState,
    readRequestForm,
    type RequestListState
  } from './requestListState';

  let {
    requestId = '',
    listState = $bindable()
  }: {
    requestId?: string;
    listState: RequestListState;
  } = $props();

  let validation = $state<string | null>(null);
  const urlFilters = $derived(
    requestFilters(readRequestForm(page.url.searchParams))
  );
  const urlProblem = $derived(
    requestProblem(readRequestForm(page.url.searchParams), true)
  );
  $effect(() => {
    void page.url.search;
    validation = null;
  });

  const requests = createQuery(() => {
    const applied = urlFilters;
    const cursor =
      requestSearch(applied) === requestSearch(listState.applied)
        ? listState.cursor
        : undefined;
    return {
      queryKey: queryKeys.requests.page(applied, cursor),
      queryFn: () => listRequests({ ...applied, cursor }),
      placeholderData: (previous) => previous,
      enabled: !requestId && !urlProblem
    };
  });

  const detail = createQuery(() => ({
    queryKey: queryKeys.requests.detail(requestId),
    queryFn: () => getRequest(requestId),
    enabled: Boolean(requestId)
  }));

  function applyFilters(event: SubmitEvent) {
    event.preventDefault();
    validation = requestProblem(listState, false, listState.applied);
    if (validation) return;
    const search = requestSearch(requestFilters(listState, listState.applied));
    if (search === page.url.searchParams.toString()) {
      Object.assign(listState, requestState(new URLSearchParams(search)));
    } else {
      void goto(resolve(`/requests${search ? `?${search}` : ''}`), {
        keepFocus: true,
        noScroll: true
      });
    }
  }

  function resetFilters() {
    validation = null;
    if (!page.url.search)
      Object.assign(listState, requestState(new URLSearchParams()));
    else void goto(resolve('/requests'), { keepFocus: true, noScroll: true });
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
  {#if requestId}<a
      class="button button-secondary"
      href={resolve(`/requests${page.url.search}`)}>Back to requests</a
    >{/if}
</div>

{#if requestId}<RequestTimeline {detail} />{:else}<RequestResults
    {requests}
    bind:listState
    {applyFilters}
    {resetFilters}
    problem={validation ?? urlProblem}
    queryProblem={urlProblem}
  />{/if}
