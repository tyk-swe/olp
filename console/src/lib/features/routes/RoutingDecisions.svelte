<script lang="ts">
  import { formatDate, formatInteger } from '#lib/format.js';
  import {
    inputBreakdownRows,
    orderedRows,
    type ExplanationRow
  } from '#lib/features/routes/routingExplanation.js';
  import InteractionInspector from './InteractionInspector.svelte';
  let { rows }: { rows: ExplanationRow[] } = $props();
  const ordered = $derived(orderedRows(rows));
  const attempted = $derived(rows.some((row) => row.attempt !== null));
  const tokens = (value: number) => `${formatInteger(value)} tokens`;
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
        {#if row.estimate && !row.contextWindow}<dt>Estimated input</dt>
          <dd>{row.estimate}</dd>{/if}
        {#if row.estimatedCost}<dt>Estimated cost</dt>
          <dd>
            {row.estimatedCost.minimum} – {row.estimatedCost.maximum}
            {row.estimatedCost
              .currency}{#if row.estimatedCost.priced_input_tokens != null && row.estimatedCost.reserved_output_tokens != null}
              · upper estimate prices {tokens(
                row.estimatedCost.priced_input_tokens
              )} in and reserves {tokens(
                row.estimatedCost.reserved_output_tokens
              )} out{/if}
          </dd>{/if}
        {#if row.maxCost !== undefined}<dt>Cost ceiling</dt>
          <dd>
            {row.maxCost === null
              ? 'Unset'
              : `${row.maxCost} ${row.estimatedCost?.currency ?? row.price?.currency ?? ''}`}
          </dd>{/if}
        {#if row.notes.length}<dt>Adaptive routing</dt>
          <dd>{row.notes.join(' · ')}</dd>{/if}
        <dt>Model facts observed</dt>
        <dd>
          {row.metadataObservedAt
            ? formatDate(row.metadataObservedAt)
            : 'Unknown'}
        </dd>
      </dl>
      {#if row.contextWindow}
        {@const {
          input_breakdown,
          remaining_tokens,
          retained_context_unknown
        } = row.contextWindow}
        <section class="context-window" aria-label="Context window">
          <h4>Context window</h4>
          <p>Planning estimates only. No billed usage.</p>
          <dl>
            <dt>Model limit</dt>
            <dd>
              {row.contextLimit == null ? 'Unknown' : tokens(row.contextLimit)}
            </dd>
            <dt>Estimated input</dt>
            <dd>
              {row.estimate ?? 'Unknown'}{#if row.estimate && !input_breakdown}
                · caller estimate{/if}
            </dd>
            <dt>Output reservation</dt>
            <dd>
              {row.contextOutput == null
                ? 'No bound applied'
                : tokens(row.contextOutput)}
            </dd>
            <dt>
              {row.contextOutput == null
                ? 'Headroom after input'
                : 'Remaining headroom'}
            </dt>
            <dd
              class:over-limit={remaining_tokens != null &&
                remaining_tokens < 0}
            >
              {#if remaining_tokens == null}Unknown
              {:else if remaining_tokens < 0}{`${tokens(-remaining_tokens)} over the limit`}
              {:else}{tokens(remaining_tokens)}{/if}
            </dd>
            {#if retained_context_unknown}
              <dt>Provider-held context</dt>
              <dd>Unknown and excluded from the input estimate</dd>
            {/if}
          </dl>
          {#if input_breakdown}
            <h5>Estimated input by category</h5>
            <dl class="context-breakdown">
              {#each inputBreakdownRows(input_breakdown) as part (part.label)}
                <dt>{part.label}</dt>
                <dd>{formatInteger(part.tokens)}</dd>
              {/each}
            </dl>
          {/if}
        </section>
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
  .context-window {
    border: 1px solid var(--border-hairline);
    border-radius: 0.5rem;
    margin: 0.75rem 0;
    padding: 0.75rem 1rem;
  }
  .context-window h4,
  .context-window h5 {
    font-size: var(--text-caption);
    margin: 0;
  }
  .context-window p {
    font-size: var(--text-caption);
  }
  .context-breakdown {
    font-variant-numeric: tabular-nums;
  }
  .over-limit {
    font-weight: 700;
  }
  @media (max-width: 36rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
