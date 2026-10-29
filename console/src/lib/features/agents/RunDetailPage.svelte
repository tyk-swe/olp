<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '$lib/api/http';
  import { getRun, listRunSpans } from './api';
  import { agentKeys } from './agentKeys';

  let { runID }: { runID: string } = $props();
  const cursor = writable<string | undefined>(undefined);
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
  const spanStatus = ['Unset', 'OK', 'Error'];
  function spanTime(nanos: string): string {
    return new Date(Number(BigInt(nanos) / 1_000_000n)).toLocaleString();
  }
</script>

<svelte:head><title>Run · OpenLLMProxy</title></svelte:head>
<a href={resolve('/runs')}>All runs</a>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>Run</h1>
  <p><code>{runID}</code></p>
</header>
{#if run.error}
  <p role="alert">{errorMessage(run.error)}</p>
{:else if run.isFetching}
  <p>Loading run…</p>
{:else if run.data}
  <dl>
    <dt>Project</dt>
    <dd><code>{run.data.project_id}</code></dd>
    <dt>API key</dt>
    <dd><code>{run.data.api_key_id}</code></dd>
    <dt>Route revision</dt>
    <dd><code>{run.data.route_revision_id ?? 'None'}</code></dd>
    <dt>Publication revision</dt>
    <dd><code>{run.data.revision_id ?? 'None'}</code></dd>
    <dt>Started</dt>
    <dd>{new Date(run.data.created_at).toLocaleString()}</dd>
  </dl>
  <section aria-labelledby="steps">
    <h2 id="steps">Steps and attempts</h2>
    <p>
      Observed amounts are accounted usage. Unresolved exposure remains reserved
      and is not a charge.
    </p>
    {#if !run.data.steps.length}
      <p>No steps recorded.</p>
    {:else}
      <div class="records">
        <table>
          <thead
            ><tr
              ><th>Step / tool</th><th>Attempt</th><th>State / outcome</th><th
                >Observed cost / charge</th
              ><th>Unresolved cost / charge</th><th>Started</th></tr
            ></thead
          >
          <tbody>
            {#each run.data.steps as step (step.id)}
              <tr>
                <td
                  >{step.ordinal + 1}. {step.kind}{#if step.tool_alias}<br
                    /><code>{step.tool_alias}</code>{/if}</td
                >
                <td><code>{step.dispatch_id}</code></td>
                <td
                  >{step.state.replaceAll('_', ' ')}{#if step.tool_status}<br
                    />Tool: {step.tool_status.replaceAll(
                      '_',
                      ' '
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
                <td>{new Date(step.created_at).toLocaleString()}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
  <section aria-labelledby="approvals">
    <h2 id="approvals">Approvals</h2>
    {#if !run.data.approvals.length}
      <p>No approvals recorded.</p>
    {:else}
      <div class="records">
        <table>
          <thead
            ><tr
              ><th>Approval</th><th>Expires</th><th>Consumed</th><th>Attempt</th
              ></tr
            ></thead
          >
          <tbody>
            {#each run.data.approvals as approval (approval.id)}
              <tr>
                <td><code>{approval.id}</code></td>
                <td>{new Date(approval.expires_at).toLocaleString()}</td>
                <td
                  >{approval.consumed_at
                    ? new Date(approval.consumed_at).toLocaleString()
                    : 'Not consumed'}</td
                >
                <td><code>{approval.dispatch_id ?? 'None'}</code></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
  <section aria-labelledby="spans">
    <h2 id="spans">Linked spans</h2>
    {#if spans.error}
      <p role="alert">{errorMessage(spans.error)}</p>
    {:else if spans.isFetching}
      <p>Loading spans…</p>
    {:else if !spans.data?.items.length}
      <p>No linked spans recorded.</p>
    {:else}
      <div class="records">
        <table>
          <thead
            ><tr
              ><th>Trace / span</th><th>Attempt</th><th>Category / status</th
              ><th>Started</th><th>Ended</th></tr
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
                <td
                  >{span.category.replaceAll('_', ' ')} / {spanStatus[
                    span.status
                  ]}</td
                >
                <td>{spanTime(span.started_nanos)}</td>
                <td>{spanTime(span.ended_nanos)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
    <nav aria-label="Span pages">
      <button
        type="button"
        disabled={!$cursor || spans.isFetching}
        onclick={() => cursor.set(undefined)}>Newest</button
      >
      <button
        type="button"
        disabled={!!spans.error || !spans.data?.nextCursor || spans.isFetching}
        onclick={() => cursor.set(spans.data?.nextCursor ?? undefined)}
        >Older</button
      >
      <button
        type="button"
        disabled={spans.isFetching}
        onclick={() => spans.refetch()}>Refresh</button
      >
    </nav>
  </section>
{/if}

<style>
  header,
  section {
    margin: 1rem 0;
  }
  dl {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: 0.5rem 1rem;
  }
  dd {
    margin: 0;
  }
  .records {
    overflow-x: auto;
  }
  table {
    width: 100%;
    text-align: left;
  }
  td,
  th {
    padding: 0.75rem;
    vertical-align: top;
  }
  code {
    overflow-wrap: anywhere;
  }
  nav {
    display: flex;
    gap: 0.5rem;
    margin-top: 1rem;
  }
</style>
