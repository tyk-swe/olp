<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import SavedFilters from '../SavedFilters.svelte';
  import { requestKeys } from '#lib/features/usage/history/requestKeys.js';

  import RequestTimeline from '#lib/features/usage/history/RequestTimeline.svelte';
  import RequestResults from '#lib/features/usage/history/RequestResults.svelte';
  import { page } from '$app/state';
  import { applyListSearch } from '#lib/listing/urlSync.svelte.js';
  import { createQuery } from '@tanstack/svelte-query';
  import { getRequest, listRequests } from '#lib/features/usage/history/api.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';

  import {
    requestFilters,
    requestProblem,
    requestSearch,
    requestUrl,
    readRequestForm,
    type RequestListState
  } from '#lib/features/usage/history/requestListState.js';

  let {
    requestId = '',
    listState = $bindable()
  }: {
    requestId?: string;
    listState: RequestListState;
  } = $props();

  const services = useServiceCapabilities();
  let validation = $state<string | null>(null);
  const urlForm = $derived(readRequestForm(page.url.searchParams));
  const urlFilters = $derived(requestFilters(urlForm));
  const urlProblem = $derived(requestProblem(urlForm, true));
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
      queryKey: requestKeys.page(applied, cursor),
      queryFn: () => listRequests({ ...applied, cursor }),
      placeholderData: (previous) => previous,
      enabled: !requestId && !urlProblem && services.retentionEnforced
    };
  });

  const detail = createQuery(() => ({
    queryKey: requestKeys.detail(requestId),
    queryFn: () => getRequest(requestId),
    enabled: Boolean(requestId) && services.retentionEnforced
  }));

  function applyFilters(event: SubmitEvent) {
    event.preventDefault();
    validation = requestProblem(listState, false, listState.applied);
    if (validation) return;
    applyListSearch(
      listState,
      requestUrl,
      requestSearch(requestFilters(listState, listState.applied))
    );
  }

  function resetFilters() {
    validation = null;
    applyListSearch(listState, requestUrl, '');
  }
</script>

<PageTitle title="Requests" />

<PageHeader
  title={requestId ? 'Request' : 'Requests'}
  description={requestId
    ? 'How this request was routed and what each upstream attempt returned. Prompts and responses are never stored.'
    : 'Every request the gateway handled. Prompts and responses are never stored.'}
/>

{#if !requestId}<SavedFilters
    scope="history"
    search={`?${requestSearch(urlFilters)}`}
    apply={(search) =>
      applyListSearch(listState, requestUrl, search.replace(/^\?/, ''))}
  />{/if}

{#if services.pending}<div class="loading-state" role="status">
    Loading request history…
  </div>
{:else if services.error}<div class="service-state">
    <Callout tone="danger">
      Request history capabilities are unavailable.
      {#snippet actions()}<button
          class="button button-secondary"
          type="button"
          onclick={() => services.retry()}
        >
          Try again
        </button>{/snippet}
    </Callout>
  </div>
{:else if !services.retentionEnforced}<div class="service-state">
    <EmptyState title="Request history is not retained">
      Request history is not retained by this installation yet, so request
      metadata cannot be explored here.
    </EmptyState>
  </div>
{:else if requestId}
  <RequestTimeline {detail} />
{:else}
  <RequestResults
    {requests}
    bind:listState
    {applyFilters}
    {resetFilters}
    problem={validation ?? urlProblem}
    queryProblem={urlProblem}
  />{/if}

<style>
  .service-state {
    margin-top: 1.5rem;
  }
</style>
