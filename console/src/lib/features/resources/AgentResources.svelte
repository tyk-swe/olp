<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { listRoutes } from '$lib/features/routes/api';
  import { routeKeys } from '$lib/features/routes/routeKeys';
  import { errorMessage } from '$lib/api/http';
  import { parseNativeJSON, nativeObject } from '$lib/json/nativeJson';
  import {
    ResourceRequestError,
    agentSubmissionFingerprint,
    runAgentRequest,
    type AgentAction
  } from './lifecycle';

  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal)
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
  let busy = $state(false);
  let nextPage = $state('');
  let submissionKey = crypto.randomUUID();
  let submittedDefinition = '';
  let controller: AbortController | undefined;
  let destroyed = false;
  onDestroy(() => {
    destroyed = true;
    controller?.abort();
    apiKey = '';
    result = '';
  });

  async function run(action: AgentAction, pageToken?: string) {
    if (busy) return;
    busy = true;
    problem = '';
    controller = new AbortController();
    try {
      if (action === 'create') {
        const fingerprint = agentSubmissionFingerprint(route, definition);
        if (fingerprint !== submittedDefinition) {
          submissionKey = crypto.randomUUID();
          submittedDefinition = fingerprint;
        }
      }
      const response = await runAgentRequest({
        action,
        apiKey,
        route,
        id,
        body: definition,
        submissionKey,
        pageToken,
        signal: controller.signal
      });
      if (destroyed) return;
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
    } catch (error) {
      if (destroyed || controller.signal.aborted) return;
      problem = errorMessage(
        error,
        'The resource request could not be completed.'
      );
      if (error instanceof ResourceRequestError && error.resourceId)
        id = error.resourceId;
    } finally {
      if (!destroyed) busy = false;
    }
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
        disabled={busy}
      /></label
    >
    <label
      >Published route<select
        bind:value={route}
        disabled={busy || routes.isPending}
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
      disabled={busy}
      spellcheck="false"></textarea></label
  >
  <div class="resource-actions">
    <button
      class="button button-primary"
      type="button"
      onclick={() => run('create')}
      disabled={busy || !route || !apiKey}>Create agent</button
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('list')}
      disabled={busy || !apiKey}>List my agents</button
    >
  </div>
  <label
    >Agent resource ID<input
      bind:value={id}
      disabled={busy}
      placeholder="agent_resource_…"
    /></label
  >
  <div class="resource-actions">
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('get')}
      disabled={busy || !id || !apiKey}>Inspect agent</button
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={() => run('delete')}
      disabled={busy || !id || !apiKey}>Delete agent</button
    >
    {#if nextPage}<button
        class="button button-secondary"
        type="button"
        onclick={() => run('list', nextPage)}
        disabled={busy}>Next page</button
      >{/if}
    {#if busy}<button
        class="button button-secondary"
        type="button"
        onclick={() => controller?.abort()}>Stop waiting</button
      >{/if}
  </div>
  {#if busy}<p role="status">Waiting for the resource operation…</p>{/if}
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
