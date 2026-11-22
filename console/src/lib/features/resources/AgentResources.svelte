<script lang="ts">
  import { useAction } from '#lib/editing/action.svelte.js';
  const runAction = useAction();
  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import {
    listRoutes,
    selectActiveRoutes,
    routeKeys
  } from '#lib/features/routes/index.js';
  import { errorMessage } from '#lib/management/http.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import { parseNativeJSON, nativeObject } from '#lib/nativeJson.js';
  import {
    ResourceRequestError,
    agentSubmissionFingerprint,
    runAgentRequest,
    type AgentAction
  } from './lifecycle';

  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal),
    select: selectActiveRoutes
  }));
  const eligible = $derived(
    (routes.data ?? []).filter((route) =>
      route.latest_revision.operations.includes('agent_resource')
    )
  );
  let apiKey = $state('');
  let route = $state('');
  let definition = $state(
    '{\n  "description": "My agent",\n  "system_instruction": "Give concise answers."\n}'
  );
  let id = $state('');
  let result = $state('');
  let problem = $state('');

  let nextPage = $state('');
  let submissionKey = crypto.randomUUID();
  let submittedDefinition = '';
  let controller: AbortController | undefined;
  let destroyed = false;
  $effect(() => {
    if (route && !eligible.some((item) => item.slug === route)) route = '';
  });
  onDestroy(() => {
    destroyed = true;
    controller?.abort();
    apiKey = '';
    result = '';
  });

  async function run(action: AgentAction, pageToken?: string) {
    if (runAction.busy) return;
    problem = '';
    controller = new AbortController();
    const owner = controller;
    await runAction.run(
      () => {
        if (action === 'create') {
          const fingerprint = agentSubmissionFingerprint(route, definition);
          if (fingerprint !== submittedDefinition) {
            submissionKey = crypto.randomUUID();
            submittedDefinition = fingerprint;
          }
        }
        return runAgentRequest({
          action,
          apiKey,
          route,
          id,
          body: definition,
          submissionKey,
          pageToken,
          signal: owner.signal
        });
      },
      (response, current) => {
        if (destroyed || !current()) return;
        result = response || 'Resource deleted.';
        nextPage = '';
        if (response) {
          const parsed = parseNativeJSON(response);
          if (nativeObject(parsed)) {
            if (typeof parsed.id === 'string') id = parsed.id;
            if (typeof parsed.next_page_token === 'string')
              nextPage = parsed.next_page_token;
          }
        }
        if (action === 'create') submittedDefinition = '';
      },
      {
        failure: (error) => {
          if (destroyed || owner.signal.aborted) return '';
          if (error instanceof ResourceRequestError && error.resourceId)
            id = error.resourceId;
          return errorMessage(
            error,
            'The resource request could not be completed.'
          );
        }
      }
    );
    if (!destroyed) problem = runAction.error;
  }
</script>

<section class="card agent-resources" aria-labelledby="agent-resources-title">
  <h2 id="agent-resources-title">Managed agents</h2>
  <p>
    Create and manage Gemini agent definitions with an inference API key. The
    key stays in this page until you leave.
  </p>
  <div class="resource-fields">
    <label
      >Inference API key<input
        type="password"
        autocomplete="off"
        bind:value={apiKey}
        disabled={runAction.busy}
      /></label
    >
    <label
      >Published route<select
        bind:value={route}
        disabled={runAction.busy || routes.isPending}
      >
        <option value="">Choose a route</option>
        {#each eligible as item (item.slug)}<option value={item.slug}
            >{item.slug}</option
          >{/each}
      </select></label
    >
  </div>
  {#if routes.isError}<p class="inline-problem" role="alert">
      {errorMessage(routes.error, 'Routes could not be loaded.')}
    </p>
  {:else if !routes.isPending && eligible.length === 0}<p>
      Publish a strict Gemini managed-agent route to enable creation.
    </p>{/if}
  <label
    >Agent definition (JSON)<textarea
      rows="7"
      bind:value={definition}
      disabled={runAction.busy}
      spellcheck="false"></textarea></label
  >
  <div class="resource-actions">
    <button
      class="button button-primary"
      type="button"
      onclick={() => run('create')}
      disabled={runAction.busy || !route || !apiKey}>Create agent</button
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('list')}
      disabled={runAction.busy || !apiKey}>List my agents</button
    >
  </div>
  <label
    >Agent resource ID<input
      bind:value={id}
      disabled={runAction.busy}
      placeholder="agent_resource_…"
    /></label
  >
  <div class="resource-actions">
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('get')}
      disabled={runAction.busy || !id || !apiKey}>Inspect agent</button
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={async () => {
        if (
          await confirmAction({
            title: `Delete agent ${id}?`,
            body: 'This asks the provider to delete the agent.',
            confirmLabel: 'Delete agent',
            danger: true
          })
        )
          await run('delete');
      }}
      disabled={runAction.busy || !id || !apiKey}>Delete agent</button
    >
    {#if nextPage}<button
        class="button button-secondary"
        type="button"
        onclick={() => run('list', nextPage)}
        disabled={runAction.busy}>Next page</button
      >{/if}
    {#if runAction.busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => controller?.abort()}>Stop waiting</button
      >{/if}
  </div>
  {#if runAction.busy}<p role="status">
      Waiting for the resource operation…
    </p>{/if}
  {#if problem}<p class="inline-problem" role="alert">{problem}</p>{/if}
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
