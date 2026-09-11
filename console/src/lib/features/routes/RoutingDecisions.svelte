<script lang="ts">
  import type { components } from '$lib/api/schema';
  let { decisions }: { decisions: components['schemas']['RoutingDecision'][] } =
    $props();
  const ordered = $derived(
    [...decisions].sort(
      (a, b) => (a.attempt ?? Infinity) - (b.attempt ?? Infinity)
    )
  );
</script>

<div class="routing-decisions" aria-label="Routing explanation">
  {#if !decisions.some((decision) => decision.attempt)}<p role="status">
      No eligible attempt matches these constraints.
    </p>{/if}
  {#each ordered as decision (`${decision.target_id}:${decision.credential_slot_id}`)}
    <article class:excluded={!decision.eligible}>
      <div class="decision-heading">
        <strong
          >{decision.attempt ? `Attempt ${decision.attempt}` : 'Excluded'} · {decision.upstream_model}</strong
        ><span class="badge">{decision.vendor_id ?? 'Custom'}</span>
      </div>
      <p>
        {decision.reason
          ? decision.reason.replaceAll('_', ' ')
          : `Priority ${decision.priority} · ${decision.strategy} routing`}
      </p>
      <dl>
        <dt>Connection</dt>
        <dd>{decision.provider_id}</dd>
        <dt>Credential slot</dt>
        <dd>{decision.credential_slot_id ?? 'Connection authentication'}</dd>
        <dt>Price</dt>
        <dd>
          {#if decision.price}{decision.price.currency} · input {decision.price
              .input_per_million ?? 'unknown'} / output {decision.price
              .output_per_million ?? 'unknown'} per million tokens{#if decision.price.unit_price}
              · {decision.price.unit_price} per unit{/if}{:else}Unknown{/if}
        </dd>
        <dt>Performance</dt>
        <dd>
          {#if decision.performance}{decision.performance.latency_ms} ms · {decision
              .performance.output_tokens_per_second ?? 'unknown'} output tokens/s
            · {decision.performance.samples} samples (observed {new Date(
              decision.performance.observed_at
            ).toLocaleTimeString()}){:else}Not enough recent measurements{/if}
        </dd>
        <dt>Model facts observed</dt>
        <dd>
          {decision.metadata_observed_at
            ? new Date(decision.metadata_observed_at).toLocaleString()
            : 'Unknown'}
        </dd>
      </dl>
    </article>
  {/each}
</div>

<style>
  article {
    border-top: 1px solid var(--border);
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
