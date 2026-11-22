<script lang="ts">
  import { formatInteger } from '#lib/format.js';
  import type { RequestAttempt } from '#lib/features/usage/history/api.js';

  let { routing }: { routing: NonNullable<RequestAttempt['routing']> } =
    $props();
</script>

{#if routing}
  <dl aria-label="Credential and routing provenance">
    {#if routing.policy}<div>
        <dt>Routing policy</dt>
        <dd class="mono">
          {routing.policy.strategy} · {routing.policy.digest}
        </dd>
      </div>
      <div>
        <dt>Vendor pin</dt>
        <dd class="mono">
          {routing.policy.vendor_id ?? 'Any vendor'}
        </dd>
      </div>
      <div>
        <dt>Pricing snapshot pinned</dt>
        <dd>
          {routing.policy.pricing_pinned ? 'Yes' : 'No'}
        </dd>
      </div>{/if}
    <div>
      <dt>Transport mode</dt>
      <dd>{routing.mode ?? 'Not recorded'}</dd>
    </div>
    <div>
      <dt>Credential source</dt>
      <dd>
        {routing.credential_source === 'caller'
          ? 'Caller supplied'
          : 'Operator held'}
      </dd>
    </div>
    {#if routing.budget_exempt}<div>
        <dt>Cost budget accounting</dt>
        <dd>Caller paid; priced usage remains recorded</dd>
      </div>{/if}
    <div>
      <dt>Credential slot</dt>
      <dd class="mono">
        {routing.credential_slot_id ?? 'Connection authentication'}
      </dd>
    </div>
    <div>
      <dt>Credential version</dt>
      <dd class="mono">
        {routing.credential_version_id ?? 'None'}
      </dd>
    </div>
    <div>
      <dt>Provider revision</dt>
      <dd class="mono">{routing.provider_revision_id}</dd>
    </div>
    <div>
      <dt>Selected price revision</dt>
      <dd class="mono">
        {routing.pricing_revision_id ?? 'Unpriced at dispatch'}
      </dd>
    </div>
    <div>
      <dt>First meaningful output</dt>
      <dd>
        {routing.first_output_ms == null
          ? 'Unknown'
          : `${routing.first_output_ms} ms`}
      </dd>
    </div>
    <div>
      <dt>Streamed output tokens</dt>
      <dd>
        {formatInteger(routing.streamed_output_tokens)}
      </dd>
    </div>
  </dl>
{/if}
