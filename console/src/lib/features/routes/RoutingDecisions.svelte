<script lang="ts">
  import {
    orderedRows,
    type ExplanationRow
  } from '$lib/features/routes/routingExplanation';
  import { stateLabel } from '$lib/format';
  import InteractionInspector from './InteractionInspector.svelte';
  let { rows }: { rows: ExplanationRow[] } = $props();
  const ordered = $derived(orderedRows(rows));
  const attempted = $derived(rows.some((row) => row.attempt !== null));
</script>

<div class="routing-decisions" aria-label="Routing explanation">
  {#if !attempted}<p role="status">
      No eligible attempt matches these constraints.
    </p>{/if}
  {#each ordered as row (row.key)}
    <article class:excluded={!row.eligible || row.attempt === null}>
      <div class="decision-heading">
        <strong
          >{row.attempt !== null ? `Attempt ${row.attempt}` : 'Excluded'} · {row.title}</strong
        >{#if row.vendorId || row.strategy}<span class="badge"
            >{row.vendorId ?? 'Custom'}</span
          >{/if}
      </div>
      <p>
        {row.reason
          ? row.reason.replaceAll('_', ' ')
          : row.priority !== null && row.strategy
            ? `Priority ${row.priority} · ${row.strategy} routing`
            : row.priority !== null
              ? `Priority ${row.priority}`
              : 'No reason recorded.'}
      </p>
      <dl>
        <dt>Connection</dt>
        <dd>{row.providerLabel}</dd>
        <dt>Credential slot</dt>
        <dd>{row.credentialSlotId ?? 'Connection authentication'}</dd>
        <dt>Price</dt>
        <dd>
          {#if row.price}{row.price.currency} · input {row.price
              .input_per_million ?? 'unknown'} / output {row.price
              .output_per_million ?? 'unknown'} per million tokens{#if row.price.unit_price}
              · {row.price.unit_price} per unit{/if}{:else}Unknown{/if}
        </dd>
        <dt>Performance</dt>
        <dd>
          {#if row.performance}{row.performance.latency_ms} ms · {row
              .performance.output_tokens_per_second ?? 'unknown'} output tokens/s
            · {row.performance.samples} samples (observed {new Date(
              row.performance.observed_at
            ).toLocaleTimeString()}){:else}Not enough recent measurements{/if}
        </dd>
        <dt>Model facts observed</dt>
        <dd>
          {row.metadataObservedAt
            ? new Date(row.metadataObservedAt).toLocaleString()
            : 'Unknown'}
        </dd>
      </dl>
      {#if row.execution || row.outcome}
        <dl aria-label="Attempt execution and outcome">
          {#if row.execution}
            <dt>Delivery</dt>
            <dd>
              upstream {row.execution.upstream_state} · client {row.execution
                .client_state} · {stateLabel(row.execution.plan_class)}
            </dd>
          {/if}
          {#if row.outcome}
            <dt>Native outcome</dt>
            <dd>{row.outcome.native_status ?? 'None observed'}</dd>
            {#if row.outcome.fault_origin}
              <dt>Fault</dt>
              <dd>
                {stateLabel(row.outcome.fault_origin)} · {stateLabel(
                  row.outcome.fault_scope
                )} scope{#if row.outcome.fault_resource}
                  · {row.outcome.fault_resource}{/if}
              </dd>
            {/if}
            {#if row.outcome.limit_category}
              <dt>Exhausted limit</dt>
              <dd>
                {stateLabel(row.outcome.limit_category)}{row.outcome.limit !=
                null
                  ? ` budget ${row.outcome.limit}`
                  : ''}
              </dd>
            {/if}
          {/if}
        </dl>
      {/if}
      <InteractionInspector
        inspection={row.interaction}
        incompatibility={row.incompatibility}
      />
    </article>
  {/each}
</div>

<style>
  article {
    border-top: 1px solid var(--border-hairline);
    padding: 1rem 0;
  }
  .decision-heading {
    display: flex;
    gap: 0.75rem;
    align-items: center;
    justify-content: space-between;
  }
  p {
    color: var(--foreground-muted);
    margin: 0.4rem 0;
  }
  dl {
    display: grid;
    grid-template-columns: 9rem minmax(0, 1fr);
    gap: 0.35rem 0.75rem;
    font-size: 0.8rem;
  }
  dt {
    color: var(--foreground-muted);
  }
  dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
  .excluded {
    opacity: 0.8;
  }
  @media (max-width: 36rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
