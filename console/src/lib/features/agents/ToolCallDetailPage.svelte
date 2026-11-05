<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useAction } from '$lib/editing/action.svelte';
  import { useMutationIntent } from '$lib/editing/mutationIntent.svelte';
  import { stateLabel } from '$lib/format';
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
        await client.invalidateQueries({ queryKey: agentKeys.allToolCalls });
        await client.invalidateQueries({ queryKey: agentKeys.allExposures });
      }
    );
  }
</script>

<svelte:head><title>Tool call · OpenLLMProxy</title></svelte:head>
<a class="text-button" href={resolve('/tool-calls')}>All tool calls</a>
<div class="page-header">
  <div>
    <p class="eyebrow">Agents</p>
    <h1 class="page-title">Tool call</h1>
  </div>
</div>
{#if call.error}
  <p class="inline-problem" role="alert">{errorMessage(call.error)}</p>
{:else if call.isPending}
  <div class="loading-state" role="status">Loading tool call…</div>
{:else if call.data}
  <p class="section">
    Outcome <span class="badge">{stateLabel(call.data.status)}</span>
  </p>
  <pre class="code-block">{JSON.stringify(call.data, null, 2)}</pre>
  {#if call.data.status === 'outcome_unknown' && access.allows('POST /api/v1/mcp-tool-calls/{call_id}/resolve')}
    <form class="card form-card" onsubmit={submit}>
      <h2>Resolve reserved amounts</h2>
      <p class="inline-status">
        Review upstream evidence before resolving this call. Resolution settles
        the held amounts or records proof that the upstream never accepted the
        call.
      </p>
      <label class="form-field"
        >Outcome<select bind:value={outcome}
          ><option value="settled">Settled</option><option value="not_accepted"
            >Not accepted</option
          ></select
        ></label
      >
      <label class="form-field"
        >Evidence SHA256 digest<input
          required
          pattern={'[a-f0-9]{64}'}
          bind:value={evidence}
        /></label
      >
      {#if outcome === 'settled'}
        <label class="form-field"
          >Observed cost<input
            required
            pattern="[0-9]+(\.[0-9]+)?"
            bind:value={cost}
          /></label
        >
        <label class="form-field"
          >Observed charge<input
            required
            pattern="[0-9]+(\.[0-9]+)?"
            bind:value={charge}
          /></label
        >
      {/if}
      <label class="check-field"
        ><input type="checkbox" required />I reviewed the evidence and amounts</label
      >
      <div class="page-actions">
        <button class="button button-primary" disabled={action.busy}
          >Resolve call</button
        >
      </div>
    </form>
  {/if}
{/if}
{#if saved}<p class="success-message" role="status">Resolution recorded</p>{/if}
{#if action.error}<p class="inline-problem" role="alert">{action.error}</p>{/if}
