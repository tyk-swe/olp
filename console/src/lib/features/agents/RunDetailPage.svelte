<script lang="ts">
  import { formatDate, stateLabel } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { getRun, listRunSpans } from './api';
  import { agentKeys } from './agentKeys';

  let { runID }: { runID: string } = $props();
  const selectedStepID = $derived(page.url.searchParams.get('step_id'));
  const selectedSpanID = $derived(page.url.searchParams.get('span_id'));
  const cursor = writable<string | undefined>(undefined);
  const access = useRole();
  const run = createQuery(() => ({
    queryKey: agentKeys.run(runID),
    queryFn: () => getRun(runID),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
  const spans = createQuery(() => ({
    queryKey: agentKeys.runSpans(runID, $cursor),
    queryFn: () => listRunSpans(runID, $cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
  const selectedSpan = createQuery(() => ({
    queryKey: agentKeys.runSpans(runID, undefined, selectedSpanID ?? undefined),
    queryFn: () => listRunSpans(runID, undefined, selectedSpanID ?? undefined),
    enabled: !!selectedSpanID && !!run.data && !run.error,
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
  const spanStatus = ['Unset', 'OK', 'Error'];
  function spanTime(nanos: string): string {
    return formatDate(
      new Date(Number(BigInt(nanos) / 1_000_000n)).toISOString()
    );
  }
</script>

<svelte:head><title>Run · OpenLLMProxy</title></svelte:head>
<a class="text-button" href={resolve('/runs')}>All runs</a>
<div class="page-header">
  <div>
    <p class="eyebrow">Agents</p>
    <h1 class="page-title">Run</h1>
    <p class="page-description"><code>{runID}</code></p>
  </div>
</div>
{#if run.error}
  <p class="inline-problem" role="alert">{errorMessage(run.error)}</p>
{:else if run.isFetching}
  <p class="inline-status" role="status">Loading run…</p>
{:else if run.data}
  {#if access.allows('POST /api/v1/investigations')}
    <p>
      <a
        class="button button-secondary"
        href={`${resolve('/investigations/new')}?project=${encodeURIComponent(run.data.project_id)}&run=${encodeURIComponent(runID)}`}
        >Save investigation</a
      >
    </p>
  {/if}
  <dl class="detail-list">
    <dt>Project</dt>
    <dd><code>{run.data.project_id}</code></dd>
    <dt>API key</dt>
    <dd><code>{run.data.api_key_id}</code></dd>
    <dt>Route revision</dt>
    <dd><code>{run.data.route_revision_id ?? 'None'}</code></dd>
    <dt>Publication revision</dt>
    <dd><code>{run.data.revision_id ?? 'None'}</code></dd>
    <dt>Started</dt>
    <dd>{formatDate(run.data.created_at)}</dd>
  </dl>
  {#if selectedSpanID}
    <section class="section" aria-labelledby="selected-span">
      <h2 id="selected-span">Selected span</h2>
      {#if selectedSpan.error}
        <p class="inline-problem" role="alert">
          {errorMessage(selectedSpan.error)}
        </p>
      {:else if selectedSpan.isFetching}
        <p class="inline-status" role="status">Loading selected span…</p>
      {:else if selectedSpan.data?.items.length}
        {@const span = selectedSpan.data.items[0]}
        <dl class="detail-list">
          <dt>Evidence ID</dt>
          <dd><code>{span.id}</code></dd>
          <dt>Trace ID</dt>
          <dd><code>{span.trace_id}</code></dd>
          <dt>Span ID</dt>
          <dd><code>{span.span_id}</code></dd>
          <dt>Attempt</dt>
          <dd><code>{span.attempt_id ?? 'Run only'}</code></dd>
          <dt>Category / status</dt>
          <dd>
            {stateLabel(span.category)} / {spanStatus[span.status]}
          </dd>
          <dt>Started</dt>
          <dd>{spanTime(span.started_nanos)}</dd>
          <dt>Ended</dt>
          <dd>{spanTime(span.ended_nanos)}</dd>
        </dl>
      {:else}
        <p>The referenced span is no longer available in this run.</p>
      {/if}
    </section>
  {/if}
  {#if selectedStepID}
    {#if run.data.steps.some((step) => step.id === selectedStepID)}
      <p>
        Referenced step <a href={`#step-${selectedStepID}`}
          ><code>{selectedStepID}</code></a
        >
      </p>
    {:else}
      <p>The referenced step is no longer available in this run.</p>
    {/if}
  {/if}
  <section class="section" aria-labelledby="steps">
    <h2 id="steps">Steps and attempts</h2>
    <p>
      Observed amounts are accounted usage. Unresolved exposure remains reserved
      and is not a charge.
    </p>
    {#if !run.data.steps.length}
      <p class="inline-status">No steps recorded.</p>
    {:else}
      <ScrollTable label="Run steps and attempts">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Step / tool</th><th scope="col">Attempt</th><th
                scope="col">State / outcome</th
              ><th scope="col">Observed cost / charge</th><th scope="col"
                >Unresolved cost / charge</th
              ><th scope="col">Started</th></tr
            ></thead
          >
          <tbody>
            {#each run.data.steps as step (step.id)}
              <tr
                id={`step-${step.id}`}
                class:selected-evidence={step.id === selectedStepID}
              >
                <td
                  >{#if step.id === selectedStepID}<strong
                      >Selected evidence</strong
                    ><br /><code>{step.id}</code><br />{/if}{step.ordinal + 1}. {step.kind}{#if step.tool_alias}<br
                    /><code>{step.tool_alias}</code>{/if}</td
                >
                <td><code>{step.dispatch_id}</code></td>
                <td
                  >{stateLabel(step.state)}{#if step.tool_status}<br />Tool: {stateLabel(
                      step.tool_status
                    )}{step.tool_is_error
                      ? ' (tool error)'
                      : ''}{/if}{#if step.bound_violation}<br />Bound exceeded{/if}</td
                >
                <td>
                  {#if step.pricing_state === 'unpriced'}
                    Unknown (unpriced)
                  {:else}
                    {step.observed_cost} / {step.observed_charge}
                    {step.currency}
                    {#if step.pricing_state === 'free'}<br />Free{/if}
                  {/if}
                </td>
                <td>
                  {#if step.pricing_state === 'unpriced'}
                    Unknown (unpriced)
                  {:else}
                    {step.remaining_cost} / {step.remaining_charge}
                    {step.currency}
                  {/if}
                </td>
                <td>{formatDate(step.created_at)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {/if}
  </section>
  <section class="section" aria-labelledby="approvals">
    <h2 id="approvals">Approvals</h2>
    {#if !run.data.approvals.length}
      <p class="inline-status">No approvals recorded.</p>
    {:else}
      <ScrollTable label="Run approvals">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Approval</th><th scope="col">Expires</th><th
                scope="col">Consumed</th
              ><th scope="col">Attempt</th></tr
            ></thead
          >
          <tbody>
            {#each run.data.approvals as approval (approval.id)}
              <tr>
                <td><code>{approval.id}</code></td>
                <td>{formatDate(approval.expires_at)}</td>
                <td
                  >{approval.consumed_at
                    ? formatDate(approval.consumed_at)
                    : 'Not consumed'}</td
                >
                <td><code>{approval.dispatch_id ?? 'None'}</code></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {/if}
  </section>
  <section class="section" aria-labelledby="spans">
    <h2 id="spans">Linked spans</h2>
    {#if spans.error}
      <p class="inline-problem" role="alert">{errorMessage(spans.error)}</p>
    {:else if spans.isFetching}
      <p class="inline-status" role="status">Loading spans…</p>
    {:else if !spans.data?.items.length}
      <p class="inline-status">No linked spans recorded.</p>
    {:else}
      <ScrollTable label="Linked spans">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Trace / span</th><th scope="col">Attempt</th><th
                scope="col">Category / status</th
              ><th scope="col">Started</th><th scope="col">Ended</th></tr
            ></thead
          >
          <tbody>
            {#each spans.data.items as span (span.id)}
              <tr>
                <td
                  ><code>{span.trace_id}</code><br /><code>{span.span_id}</code
                  >{#if span.parent_span_id}<br />Parent
                    <code>{span.parent_span_id}</code>{/if}</td
                >
                <td><code>{span.attempt_id ?? 'Run only'}</code></td>
                <td>{stateLabel(span.category)} / {spanStatus[span.status]}</td>
                <td>{spanTime(span.started_nanos)}</td>
                <td>{spanTime(span.ended_nanos)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {/if}
    <nav class="page-actions list-pager" aria-label="Span pages">
      <button
        class="button button-secondary"
        type="button"
        disabled={!$cursor || spans.isFetching}
        onclick={() => cursor.set(undefined)}>Newest</button
      >
      <button
        class="button button-secondary"
        type="button"
        disabled={!!spans.error || !spans.data?.nextCursor || spans.isFetching}
        onclick={() => cursor.set(spans.data?.nextCursor ?? undefined)}
        >Older</button
      >
      <button
        class="button button-secondary"
        type="button"
        disabled={spans.isFetching}
        onclick={() => spans.refetch()}>Refresh</button
      >
    </nav>
  </section>
{/if}

<style>
  .selected-evidence {
    background: var(--accent-soft);
  }
</style>
