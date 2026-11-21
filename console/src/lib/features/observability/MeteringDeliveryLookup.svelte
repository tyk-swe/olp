<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { formatDate } from '#lib/format.js';
  import { getMeteringDelivery } from './api';
  import { observabilityKeys } from './observabilityKeys';
  import { meteringDeliveryLabel } from './presentation';
  let { sinkId }: { sinkId: string } = $props();
  let attemptInput = $state('');
  let submitted = $state('');
  const lookup = createQuery(() => ({
    queryKey: observabilityKeys.meteringDelivery(sinkId, submitted),
    queryFn: ({ signal }) => getMeteringDelivery(sinkId, submitted, signal),
    enabled: submitted !== ''
  }));
  function submit(event: SubmitEvent) {
    event.preventDefault();
    submitted = attemptInput.trim();
  }
</script>

<form class="create-form" onsubmit={submit}>
  <div class="form-field">
    <label for="metering-attempt">Attempt ID</label>
    <input
      id="metering-attempt"
      class="mono"
      bind:value={attemptInput}
      required
      placeholder="Attempt ID, also the CloudEvents event ID"
    />
    <small
      >Accepted means this sink answered 2xx and OLP recorded it. It says
      nothing about downstream ledger posting.</small
    >
  </div>
  <div class="form-actions">
    <button
      class="button button-secondary"
      type="submit"
      disabled={!attemptInput.trim() || lookup.isFetching}>Look up</button
    >
  </div>
</form>
{#if submitted}
  {#if lookup.isPending}<span class="inline-status" role="status"
      >Looking up delivery…</span
    >
  {:else if lookup.isError}<span class="inline-problem" role="alert"
      >Delivery lookup is unavailable.
      <button class="text-button" type="button" onclick={() => lookup.refetch()}
        >Retry</button
      ></span
    >
  {:else if lookup.data}
    {@const delivery = lookup.data}
    <dl class="delivery-lookup">
      <dt>Status</dt>
      <dd>{meteringDeliveryLabel(delivery.outcome)}</dd>
      <dt>Attempt</dt>
      <dd class="mono">{delivery.attempt_id}</dd>
      {#if delivery.event_id}<dt>Event ID</dt>
        <dd class="mono">{delivery.event_id}</dd>{/if}
      {#if delivery.attempts != null}<dt>Delivery claims</dt>
        <dd>{delivery.attempts}</dd>{/if}
      {#if delivery.last_error_code}<dt>Last error</dt>
        <dd class="mono">{delivery.last_error_code}</dd>{/if}
      {#if delivery.occurred_at}<dt>Observed</dt>
        <dd>{formatDate(delivery.occurred_at)}</dd>{/if}
      {#if delivery.queued_at}<dt>Queued</dt>
        <dd>{formatDate(delivery.queued_at)}</dd>{/if}
      {#if delivery.last_attempt_at}<dt>Last attempt</dt>
        <dd>{formatDate(delivery.last_attempt_at)}</dd>{/if}
      {#if delivery.settled_at}<dt>Settled</dt>
        <dd>{formatDate(delivery.settled_at)}</dd>{/if}
    </dl>
  {/if}
{/if}
