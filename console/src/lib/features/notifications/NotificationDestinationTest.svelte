<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import {
    readDestinationTest,
    sendDestinationTest,
    type DestinationTestView,
    type NotificationDestination
  } from './api';

  let { destination }: { destination: NotificationDestination } = $props();
  const action = useAction();
  let open = $state(false);
  let confirming = $state(false);
  let actionId = $state('');
  let result = $state.raw<DestinationTestView | null>(null);

  usePrincipalLifetime(() => {
    open = confirming = false;
    actionId = '';
    result = null;
  });

  function show() {
    open = true;
    confirming = !actionId;
  }

  function send(id: string) {
    if (action.busy) return;
    actionId = id;
    confirming = false;
    result = null;
    return action.run(
      () => sendDestinationTest(destination.id, id),
      (value) => {
        result = value;
      }
    );
  }

  function confirm() {
    if (confirming && !action.busy) return send(crypto.randomUUID());
  }

  function refresh() {
    const id = actionId;
    return action.run(
      () => readDestinationTest(destination.id, id),
      (value) => {
        result = value;
      }
    );
  }

  $effect(() => {
    if (!open || confirming || result?.status !== 'pending' || action.busy)
      return;
    const timer = setTimeout(() => void refresh(), 1500);
    return () => clearTimeout(timer);
  });
</script>

<button class="text-button" type="button" onclick={show}>
  {actionId ? 'Test status' : 'Send test'}
</button>

{#if open}
  <Dialog
    title="Test notification"
    description={`Send a labeled test message to ${destination.name} using its saved settings.`}
    onClose={() => (open = false)}
  >
    {#snippet children(close)}
      {#if action.error}<Callout tone="danger">{action.error}</Callout>{/if}
      {#if confirming}
        <p>
          {actionId
            ? 'A new test could deliver another message.'
            : 'This does not trigger any notification rules.'}
        </p>
        <div class="test-actions">
          <button class="button" type="button" data-autofocus onclick={close}
            >Cancel</button
          >
          <button class="button button-primary" type="button" onclick={confirm}>
            Send test
          </button>
        </div>
      {:else}
        <div role="status">
          {#if result?.status === 'delivered'}
            <p>Test delivered.</p>
          {:else if result?.status === 'unconfirmed'}
            <p>
              Delivery unconfirmed. The receiver may have accepted the test.
            </p>
          {:else if result?.status === 'failed'}
            <p>Test delivery failed.</p>
          {:else if result?.status === 'missing'}
            <p>
              Result pending. No record found yet. You can retry the same test.
            </p>
          {:else if result?.status === 'pending'}
            <p>Result pending.</p>
          {:else if action.busy}
            <p>Sending test…</p>
          {/if}
          {#if result?.failure}<p>
              Error code: <code>{result.failure}</code>
            </p>{/if}
        </div>
        <div class="test-actions">
          <button
            class="button"
            type="button"
            disabled={action.busy}
            onclick={refresh}>Refresh status</button
          >
          {#if result?.status === 'missing' || (!result && !action.busy)}
            <button
              class="button"
              type="button"
              disabled={action.busy}
              onclick={() => send(actionId)}>Retry same test</button
            >
          {:else if result && result.status !== 'pending'}
            <button
              class="button"
              type="button"
              disabled={action.busy}
              onclick={() => (confirming = true)}>Start a new test</button
            >
          {/if}
        </div>
      {/if}
    {/snippet}
  </Dialog>
{/if}

<style>
  .test-actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: end;
    gap: 0.5rem;
    margin-top: 1rem;
  }
</style>
