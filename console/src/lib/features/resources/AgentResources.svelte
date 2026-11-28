<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import QueryView from '#lib/components/QueryView.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { captureResult } from '#lib/editing/result.js';
  import { selectActiveRoutes } from '#lib/domain/routes/activeRoutes.js';
  import { routeQueries } from '#lib/domain/routes/queries.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    ResourceRequestError,
    agentSubmissionFingerprint,
    runAgentRequest,
    type AgentAction
  } from './lifecycle';

  const mutation = useMutation();
  const routes = createQuery(() => ({
    ...routeQueries.all(),
    select: selectActiveRoutes
  }));
  const eligible = $derived(
    (routes.data ?? []).filter((item) =>
      item.latest_revision.operations.includes('agent_resource')
    )
  );
  let apiKey = $state('');
  let route = $state('');
  let definition = $state(
    '{\n  "description": "My agent",\n  "system_instruction": "Give concise answers."\n}'
  );
  let id = $state('');
  let result = $state('');
  let nextPage = $state('');
  let controller: AbortController | undefined;
  const canCreate = $derived(
    !routes.isPending &&
      !routes.isError &&
      eligible.some((item) => item.slug === route) &&
      !!apiKey.trim()
  );

  usePrincipalLifetime(() => {
    controller?.abort();
    controller = undefined;
    apiKey = route = id = result = nextPage = '';
  });

  $effect(() => {
    if (route && !eligible.some((item) => item.slug === route)) route = '';
  });

  async function run(action: AgentAction, pageToken?: string) {
    if (mutation.busy || (action === 'create' && !canCreate)) return;
    const fingerprint =
      action === 'create'
        ? captureResult(() => agentSubmissionFingerprint(route, definition))
        : null;
    if (fingerprint && !fingerprint.ok) {
      mutation.error = errorMessage(
        fingerprint.error,
        'The resource request could not be completed.'
      );
      return;
    }
    const owner = new AbortController();
    controller = owner;
    const request = {
      action,
      apiKey,
      route,
      id,
      body: definition,
      pageToken,
      signal: owner.signal
    };
    const accept = (response: Awaited<ReturnType<typeof runAgentRequest>>) => {
      result = response.text || 'Resource deleted.';
      if (response.id) id = response.id;
      nextPage = response.nextPageToken ?? '';
    };
    const feedback = {
      key: action,
      failure: (cause: unknown) => {
        if (owner.signal.aborted) return '';
        if (cause instanceof ResourceRequestError && cause.resourceId)
          id = cause.resourceId;
        return errorMessage(
          cause,
          'The resource request could not be completed.'
        );
      }
    };
    if (fingerprint) {
      await mutation.submit(
        [fingerprint.value, apiKey.trim()],
        (_intent, requestId) =>
          runAgentRequest({ ...request, submissionKey: requestId }),
        accept,
        feedback
      );
    } else {
      await mutation.run(() => runAgentRequest(request), accept, feedback);
    }
    if (controller === owner) controller = undefined;
  }

  const confirmDelete = async () => {
    if (
      await confirmAction({
        title: `Delete agent ${id}?`,
        body: 'This asks the provider to delete the agent.',
        confirmLabel: 'Delete agent',
        danger: true
      })
    )
      await run('delete');
  };
</script>

<section class="card agent-resources" aria-labelledby="agent-resources-title">
  <h2 id="agent-resources-title">Managed agents</h2>
  <p>
    Create and manage Gemini agent definitions with an inference API key. The
    key stays in this page until you leave.
  </p>
  <div class="resource-fields">
    <label>
      Inference API key<input
        type="password"
        autocomplete="off"
        bind:value={apiKey}
        disabled={mutation.busy}
      />
    </label>
    <QueryView
      query={routes}
      loading="Loading published routes…"
      failure={errorMessage(routes.error, 'Routes could not be loaded.')}
      emptyWhen={eligible.length === 0}
    >
      {#snippet empty()}
        <p class="muted">
          Publish a strict Gemini managed-agent route to enable creation.
        </p>
      {/snippet}
      <label>
        Published route<select bind:value={route} disabled={mutation.busy}>
          <option value="">Choose a route</option>
          {#each eligible as item (item.slug)}<option value={item.slug}>
              {item.slug}
            </option>{/each}
        </select>
      </label>
    </QueryView>
  </div>
  <label>
    Agent definition (JSON)<textarea
      rows="7"
      bind:value={definition}
      disabled={mutation.busy}
      spellcheck="false"></textarea>
  </label>
  <div class="resource-actions">
    <button
      class="button button-primary"
      type="button"
      onclick={() => run('create')}
      disabled={mutation.busy || !canCreate}
    >
      Create agent
    </button>
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('list')}
      disabled={mutation.busy || !apiKey}
    >
      List my agents
    </button>
  </div>
  <label>
    Agent resource ID<input
      bind:value={id}
      disabled={mutation.busy}
      placeholder="agent_resource_…"
    />
  </label>
  <div class="resource-actions">
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('get')}
      disabled={mutation.busy || !id || !apiKey}
    >
      Inspect agent
    </button>
    <button
      class="button button-secondary"
      type="button"
      onclick={confirmDelete}
      disabled={mutation.busy || !id || !apiKey}
    >
      Delete agent
    </button>
    {#if nextPage}<button
        class="button button-secondary"
        type="button"
        onclick={() => run('list', nextPage)}
        disabled={mutation.busy}
      >
        Next page
      </button>{/if}
    {#if mutation.busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => controller?.abort()}
      >
        Stop waiting
      </button>{/if}
  </div>
  {#if mutation.busy}<p role="status">
      Waiting for the resource operation…
    </p>{/if}
  {#if mutation.error}<p class="inline-problem" role="alert">
      {mutation.error}
    </p>{/if}
  {#if result}<pre aria-label="Agent resource result">{result}</pre>{/if}
</section>

<style>
  .agent-resources {
    display: grid;
    gap: 1rem;
    margin-block: 1rem;
  }
  .resource-fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: 1rem;
  }
  label {
    display: grid;
    gap: 0.4rem;
  }
  .resource-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.6rem;
  }
  textarea,
  pre {
    font-family: var(--font-mono, monospace);
  }
  pre {
    max-height: 28rem;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
