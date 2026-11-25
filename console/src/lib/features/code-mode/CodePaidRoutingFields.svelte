<script lang="ts">
  import PaidTargetSelectors from './PaidTargetSelectors.svelte';
  import type { CodeRouteWrite } from '#lib/features/code-mode/api.js';
  import type { PaidTargetOption } from './presentation';

  type PaidTargets = NonNullable<CodeRouteWrite['paid_targets']>;

  let {
    paidParticipation = $bindable(),
    paidTargets = $bindable(),
    paidFallbackTargets = $bindable(),
    models,
    providers
  }: {
    paidParticipation: boolean;
    paidTargets: PaidTargets;
    paidFallbackTargets: PaidTargets;
    models: string[];
    providers: PaidTargetOption[];
  } = $props();
</script>

<label>
  <input
    id="code-paid-participation"
    type="checkbox"
    bind:checked={paidParticipation}
  />Permit paid API targets
</label>
<p>
  Mapped models directly use the selected metered API target, even when a
  subscription supports the same model. Fallback targets are separate and keep
  subscription candidates first. Known pricing and every applicable spend limit
  are required on the actual paid attempt.
</p>
{#if paidParticipation}
  <PaidTargetSelectors
    targets={paidTargets}
    prefix="paid"
    label="Paid API"
    emptyLabel="Use the subscription pool"
    onchange={(targets) => (paidTargets = targets)}
    {models}
    {providers}
  />
  <p>
    Subscription-first paid fallback requires bounded recovery, proven
    nonexecution and portable complete input. The exact model is preferred. A
    different model also requires model rewriting and an explicit model pool.
  </p>
  <PaidTargetSelectors
    targets={paidFallbackTargets}
    prefix="fallback"
    label="Paid fallback"
    emptyLabel="No paid fallback"
    onchange={(targets) => (paidFallbackTargets = targets)}
    {models}
    {providers}
  />
  <small>
    Paid targets use their native HTTP protocol. Publish after changing a
    provider or slot. Targets with request defaults, caller credentials or
    subscription adapters are unsupported.
  </small>
{/if}
