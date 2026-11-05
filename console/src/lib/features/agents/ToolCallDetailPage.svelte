<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useAction } from '$lib/editing/action.svelte';
  import { useMutationIntent } from '$lib/editing/mutationIntent.svelte';
  import { errorMessage } from '$lib/management/http';
  import { getToolCall, resolveToolCall } from './api';
  import { agentKeys } from './agentKeys';
  let { callID }: { callID: string } = $props();
  const access = useRole();
  const client = useQueryClient();
  const action = useAction();
  const intent = useMutationIntent();
  const call = createQuery(() => ({
    queryKey: agentKeys.call(callID),
    queryFn: () => getToolCall(callID),
    retry: false
  }));
  let outcome = $state<'settled' | 'not_accepted'>('settled');
  let evidence = $state('');
  let cost = $state('0');
  let charge = $state('0');
  let saved = $state(false);
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const body = {
      outcome,
      evidence_digest: evidence,
      ...(outcome === 'settled'
        ? { observed_cost: cost, observed_charge: charge }
        : {})
    };
    await action.run(
      () =>
        intent.submit({ id: callID, body }, (input, requestId) =>
          resolveToolCall(input.id, input.body, requestId)
        ),
      async () => {
        saved = true;
        await client.invalidateQueries({ queryKey: agentKeys.call(callID) });
        await client.invalidateQueries({ queryKey: ['mcp-tool-calls'] });
        await client.invalidateQueries({ queryKey: ['execution-debits'] });
      }
    );
  }
</script>

<svelte:head><title>Tool call · OpenLLMProxy</title></svelte:head>
<section class="workflow">
  <a href={resolve('/tool-calls')}>All tool calls</a>
  <h1 class="page-title">Tool call</h1>
  {#if call.error}<p role="alert">{errorMessage(call.error)}</p>{/if}
  {#if call.data}
    <p>Outcome {call.data.status}</p>
    <pre>{JSON.stringify(call.data, null, 2)}</pre>
    {#if call.data.status === 'outcome_unknown' && access.allows('POST /api/v1/mcp-tool-calls/{call_id}/resolve')}
      <form class="card" onsubmit={submit}>
        <h2>Resolve reserved amounts</h2>
        <p>
          Review upstream evidence before resolving this call. Resolution
          settles the held amounts or records proof that the upstream never
          accepted the call
        </p>
        <label
          >Outcome<select bind:value={outcome}
            ><option value="settled">Settled</option><option
              value="not_accepted">Not accepted</option
            ></select
          ></label
        >
        <label
          >Evidence SHA256 digest<input
            required
            pattern={'[a-f0-9]{64}'}
            bind:value={evidence}
          /></label
        >
        {#if outcome === 'settled'}
          <label
            >Observed cost<input
              required
              pattern="[0-9]+(\.[0-9]+)?"
              bind:value={cost}
            /></label
          >
          <label
            >Observed charge<input
              required
              pattern="[0-9]+(\.[0-9]+)?"
              bind:value={charge}
            /></label
          >
        {/if}
        <label
          ><input type="checkbox" required />I reviewed the evidence and amounts</label
        >
        <button class="button button-primary" disabled={action.busy}
          >Resolve call</button
        >
      </form>
    {/if}
  {/if}
  {#if saved}<p role="status">Resolution recorded</p>{/if}
  {#if action.error}<p role="alert">{action.error}</p>{/if}
</section>
