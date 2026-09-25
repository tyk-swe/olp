<script lang="ts">
  import type { components } from '$lib/api/schema';
  import { stateLabel } from '$lib/format';

  type Inspection = components['schemas']['InteractionInspection'];
  type Incompatibility = components['schemas']['InteractionIncompatibility'];
  let {
    inspection,
    incompatibility
  }: {
    inspection?: Inspection | null;
    incompatibility?: Incompatibility | null;
  } = $props();

  const effective = $derived(inspection?.effective_request);
  const obligations = $derived(inspection?.obligations);
  const hostedTools = $derived(inspection?.hosted_tools ?? []);
  const hasDetails = $derived(
    Boolean(
      effective ||
      inspection?.semantic_context?.length ||
      inspection?.dispositions?.length ||
      obligations ||
      hostedTools.length
    )
  );
  const status = $derived(inspection?.status ?? 'not_inspected');
  const evidence = $derived(inspection?.evidence ?? []);

  function label(value: string | undefined | null) {
    return value ? stateLabel(value) : 'Not established';
  }
</script>

{#if inspection || incompatibility}
  <div class="inspection" aria-label="Effective interaction plan">
    <p class="plan-status">
      <strong>{label(status)}</strong>
      · {label(inspection?.fidelity)} route
      {#if inspection?.class}
        · {label(inspection.class)} plan{/if}
    </p>
    {#if incompatibility}
      <p class="incompatibility" role="status">
        {label(incompatibility.code)} at <code>{incompatibility.field}</code>:
        {incompatibility.message}
      </p>
    {/if}
    {#if status === 'not_inspected'}
      <p class="scope-note">
        Tuple eligibility only. Supply a native request to inspect the effective
        invocation and return contract.
      </p>
    {:else if status === 'legacy'}
      <p class="scope-note">
        Historical route behavior is shown without a strict interaction
        qualification.
      </p>
    {:else}
      <p class="scope-note">
        Planner result only. Inspection does not send provider inference, create
        jobs, or run tools. Admission is not a quality test.
      </p>
    {/if}
    {#if hasDetails}
      <details>
        <summary>Inspect request, return path, and obligations</summary>
        <dl class="contract">
          <dt>Operation</dt>
          <dd>{label(inspection?.operation)}</dd>
          <dt>Ingress</dt>
          <dd>{label(inspection?.ingress_dialect)}</dd>
          <dt>OIF</dt>
          <dd>
            {inspection?.representation === 'oif'
              ? 'Source-preserving operation document'
              : 'Not established'}
          </dd>
          <dt>Egress</dt>
          <dd>{label(inspection?.egress_dialect)}</dd>
          <dt>Return</dt>
          <dd>{label(inspection?.return_dialect)}</dd>
          <dt>Profile</dt>
          <dd>
            {inspection?.profile_id ??
              'Not established'}{#if inspection?.profile_revision}
              · revision {inspection.profile_revision}{/if}
          </dd>
          <dt>Hosted tools</dt>
          <dd>
            {hostedTools.length
              ? hostedTools.map(label).join(', ')
              : 'None admitted'}
          </dd>
          {#if inspection?.serving}
            <dt>Serving binding</dt>
            <dd>
              {inspection.serving.model} · provider revision
              {inspection.serving.provider_revision_id}
            </dd>
            <dt>Environment</dt>
            <dd>
              Principal {inspection.serving.principal_declared
                ? 'declared'
                : 'unknown'} · region {inspection.serving.region_declared
                ? 'declared'
                : 'unknown'} · resource scope {inspection.serving
                .resource_scope_declared
                ? 'declared'
                : 'unknown'} · snapshot {inspection.serving.snapshot_declared
                ? 'declared'
                : 'unknown'}
            </dd>
          {/if}
        </dl>
        {#if effective?.structure?.length}
          <h4>Ordered input structure</h4>
          <ol class="structure">
            {#each effective.structure as turn (`${turn.scope}-${turn.index}`)}
              <li>
                <strong
                  >{label(turn.scope)}
                  {turn.index + 1} · {label(turn.role)}</strong
                >
                <span>
                  {#each turn.parts as part, index (index)}
                    {#if index > 0}
                      →
                    {/if}{label(part.kind)}{#if part.call_ordinal}
                      {part.call_ordinal}{/if}
                  {/each}
                  {#if turn.omitted_parts}
                    · {turn.omitted_parts} further blocks hidden
                  {/if}
                </span>
              </li>
            {/each}
          </ol>
          {#if effective.omitted_turns}<p class="scope-note">
              {effective.omitted_turns} further input entries hidden.
            </p>{/if}
          <p class="scope-note">
            Tool numbers link results to earlier calls when the native request
            carries an explicit ID. Content, tool names, arguments, schemas,
            IDs, and opaque state stay hidden.
          </p>
        {/if}
        {#if effective?.fields?.length}
          <h4>Effective native fields</h4>
          <div
            class="table-shell"
            role="region"
            aria-label="Effective native fields"
          >
            <table class="data-table">
              <thead
                ><tr
                  ><th>Field</th><th>Presence</th><th>Origin</th><th
                    >Safe value</th
                  ></tr
                ></thead
              >
              <tbody>
                {#each effective.fields as field (`${field.field}-${field.kind}`)}
                  <tr>
                    <td><code>{field.field}</code></td>
                    <td>{field.kind}{field.empty ? ' · empty' : ''}</td>
                    <td>{label(field.origin)}</td>
                    <td
                      >{#if field.value_json !== undefined}<code
                          >{field.value_json}</code
                        >{:else}Hidden{/if}</td
                    >
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
          {#if effective.redacted_native_fields}<p class="scope-note">
              {effective.redacted_native_fields} unknown native fields hidden with
              their names.
            </p>{/if}
        {/if}
        {#if inspection?.semantic_context?.length}
          <h4>Semantic context</h4>
          <ul>
            {#each inspection.semantic_context as field (field.field)}<li>
                <code>{field.field}</code> · {label(field.origin)} · value hidden
              </li>{/each}
          </ul>
        {/if}
        {#if inspection?.dispositions?.length}
          <h4>Provenance and transformations</h4>
          <ul>
            {#each inspection.dispositions as item, index (index)}<li>
                <code>{item.field}</code> · {label(item.disposition)} ·
                {label(item.rule)} · {label(item.evidence)}
              </li>{/each}
          </ul>
          {#if inspection.omitted_dispositions}<p class="scope-note">
              {inspection.omitted_dispositions} further dispositions collapsed or
              hidden.
            </p>{/if}
        {/if}
        {#if obligations}
          <h4>Execution and client obligations</h4>
          <dl class="contract">
            <dt>Delivery</dt>
            <dd>{label(obligations.delivery)}</dd>
            <dt>Lifetime / submission</dt>
            <dd>
              {label(obligations.lifetime)} · {label(obligations.submission)}
            </dd>
            <dt>Continuation</dt>
            <dd>{label(obligations.continuation)}</dd>
            <dt>Retry</dt>
            <dd>{label(obligations.retry)}</dd>
            <dt>Effects</dt>
            <dd>
              {obligations.effects.map(label).join(', ') || 'None declared'}
            </dd>
            <dt>Actionability</dt>
            <dd>{label(obligations.actionability)}</dd>
            <dt>Planner bounds</dt>
            <dd>
              Body {obligations.max_body_bytes} bytes · event {obligations.max_event_bytes}
              bytes{#if obligations.max_continuation_bytes}
                · continuation {obligations.max_continuation_bytes} bytes{/if}
            </dd>
            <dt>Result guard</dt>
            <dd>{obligations.guard_results ? 'Required' : 'None declared'}</dd>
            <dt>Ambiguous failover</dt>
            <dd>
              {obligations.reject_ambiguous_failover
                ? 'Rejected'
                : 'Contract dependent'}
            </dd>
          </dl>
        {/if}
      </details>
    {/if}
    <p class="scope-note">
      Evidence scope: {evidence.length
        ? evidence.map(label).join(' · ')
        : 'None recorded'}. Provider connectivity, declared capability,
      deterministic plan admission, and empirical quality are separate evidence.
    </p>
  </div>
{/if}

<style>
  .inspection {
    border-top: 1px solid var(--border-hairline);
    margin-top: 0.75rem;
    padding-top: 0.75rem;
    font-size: 0.82rem;
  }
  .plan-status,
  .scope-note {
    margin: 0.25rem 0 0.6rem;
  }
  .scope-note {
    color: var(--foreground-muted);
  }
  .incompatibility {
    color: var(--danger);
  }
  details {
    margin: 0.65rem 0;
  }
  summary {
    cursor: pointer;
    font-weight: 550;
  }
  h4 {
    margin: 1rem 0 0.45rem;
    font-size: 0.85rem;
  }
  .contract {
    display: grid;
    grid-template-columns: minmax(8rem, 0.35fr) minmax(0, 1fr);
    gap: 0.4rem 0.8rem;
    margin: 0.7rem 0;
  }
  .contract dt {
    color: var(--foreground-muted);
  }
  .contract dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
  .structure {
    padding-left: 1.2rem;
  }
  .structure li {
    padding: 0.25rem 0;
  }
  .structure span {
    display: block;
    color: var(--foreground-muted);
  }
  code {
    font-family: var(--font-mono);
    overflow-wrap: anywhere;
  }
  @media (max-width: 38rem) {
    .contract {
      grid-template-columns: 1fr;
    }
  }
</style>
