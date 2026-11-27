<script lang="ts" generics="T">
  import { useAction } from '#lib/editing/action.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  let {
    label,
    description,
    identity,
    run,
    done = () => {},
    disabled = false,
    danger = false,
    action = useAction()
  }: {
    label: string;
    description: string;
    identity: string;
    run: (requestId: string) => Promise<T>;
    done?: (result: T) => void | Promise<void>;
    disabled?: boolean;
    danger?: boolean;
    action?: ReturnType<typeof useAction>;
  } = $props();
  let confirming = $state(false);
  const intent = useMutationIntent();
  usePrincipalLifetime(() => {
    confirming = false;
  });
</script>

{#if confirming}
  <span class="confirm-bar">
    <span class="inline-status">{description}</span>
    <button
      class="button button-secondary"
      class:button-danger={danger}
      type="button"
      disabled={disabled || action.busy}
      onclick={() =>
        action.run(
          () => intent.submit(identity, (_, requestId) => run(requestId)),
          async (result) => {
            confirming = false;
            await done(result);
          }
        )}
    >
      Confirm {label.toLowerCase()}
    </button>
    <button
      class="button button-secondary"
      type="button"
      disabled={action.busy}
      onclick={() => {
        confirming = false;
        intent.reset();
      }}
    >
      Cancel
    </button>
  </span>
{:else}
  <button
    class="button button-secondary"
    class:button-danger={danger}
    type="button"
    disabled={disabled || action.busy}
    onclick={() => {
      confirming = true;
    }}
  >
    {label}
  </button>
{/if}
{#if action.error}<p class="inline-problem" role="alert">{action.error}</p>{/if}

<style>
  .confirm-bar {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
  }
</style>
