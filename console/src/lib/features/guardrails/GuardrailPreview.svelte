<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { apiKeyKeys, listApiKeys } from '#lib/features/access/index.js';
  import {
    listOperationDialects,
    listRoutes,
    routeKeys,
    selectActiveRoutes
  } from '#lib/features/routes/index.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    getGuardrailPreview,
    type PreviewEntry,
    type PreviewSelection
  } from './api';
  import { guardrailKeys } from './guardrailKeys';

  const generation = [
    ['openai-chat', 'OpenAI Chat Completions'],
    ['openai-responses', 'OpenAI Responses'],
    ['openai-completions', 'OpenAI Completions'],
    ['anthropic-messages', 'Anthropic Messages'],
    ['gemini-generate-content', 'Gemini Generate Content'],
    ['gemini-interactions', 'Gemini Interactions'],
    ['bedrock-converse', 'Bedrock Converse'],
    ['mistral-fim', 'Mistral Fill-in-the-Middle'],
    ['cohere-chat-v2', 'Cohere Chat']
  ] as const;
  const uninspected: Readonly<Record<string, readonly string[]>> = {
    batch: ['openai-batch', 'anthropic-batch', 'gemini-batch', 'cloud-batch'],
    bedrock_invoke: ['bedrock-invoke'],
    realtime: ['openai-realtime', 'gemini-live', 'native-duplex'],
    passthrough: ['native']
  };
  const transformed = [
    'openai-embeddings',
    'openai-moderation',
    'rerank',
    'openai-input-tokens',
    'anthropic-count-tokens',
    'gemini-count-tokens'
  ];
  const partialNotice =
    'This preview shows only guardrails you can view. Full-policy compatibility is not shown.';
  const notes: Record<PreviewEntry['notes'][number], string> = {
    rewrite_unavailable: 'This surface cannot apply the configured rewrite.',
    retained_output_uninspected:
      'This guardrail is incompatible with provider-retained output.',
    streaming_unavailable: 'This guardrail cannot inspect the selected stream.',
    surface_uninspected: 'This surface cannot run this guardrail.'
  };

  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    notifyOnChangeProps: 'all',
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  const keys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    notifyOnChangeProps: 'all',
    queryFn: ({ signal }) => listApiKeys(signal)
  }));
  const dialects = createQuery(() => ({
    queryKey: routeKeys.dialects(),
    notifyOnChangeProps: 'all',
    queryFn: ({ signal }) => listOperationDialects(signal)
  }));
  let routeId = $state('');
  let keyId = $state('');
  let operation = $state('');
  let surface = $state('');
  let delivery = $state<PreviewSelection['mode']>('unary');
  let retainsOutput = $state(false);
  let submitted = $state<PreviewSelection | null>(null);

  usePrincipalLifetime(() => {
    routeId = '';
    clearRouteSelection();
    submitted = null;
  });

  const activeRoutes = $derived(selectActiveRoutes(routes.data ?? []));
  const selectedRoute = $derived(
    activeRoutes.find((item) => item.id === routeId)
  );
  const availableKeys = $derived(
    selectedRoute
      ? (keys.data ?? []).filter(
          (key) => (key.project_id ?? null) === selectedRoute.project_id
        )
      : []
  );
  const operationDialects = $derived(
    (dialects.data?.items ?? []).filter(
      (item) =>
        item.operation === operation &&
        (selectedRoute?.latest_revision.fidelity.mode === 'strict' ||
          ['decision', 'ocr', 'search'].includes(operation) ||
          transformed.includes(item.id))
    )
  );
  const surfaces = $derived(
    operation === 'generation'
      ? generation.map(([id, label]) => ({ id, label }))
      : [
          ...operationDialects.map(({ id, label }) => ({ id, label })),
          ...(uninspected[operation] ?? []).map((id) => ({ id, label: id })),
          ...(!uninspected[operation] && operation && !operationDialects.length
            ? [{ id: 'openai', label: 'OpenAI' }]
            : [])
        ]
  );
  const canStream = $derived(
    [
      'generation',
      'bedrock_invoke',
      'image_generation',
      'image_edit',
      'speech',
      'transcription'
    ].includes(operation)
  );
  const preview = createQuery(() => ({
    queryKey: [...guardrailKeys.root, 'preview', submitted] as const,
    queryFn: ({ queryKey: [, , input], signal }) =>
      input ? getGuardrailPreview(input, signal) : null,
    enabled: submitted !== null,
    notifyOnChangeProps: 'all',
    retry: false,
    gcTime: 0,
    refetchOnWindowFocus: false
  }));

  function clearRouteSelection() {
    keyId = '';
    operation = '';
    clearSurfaceSelection();
  }

  function clearSurfaceSelection() {
    surface = '';
    delivery = 'unary';
    retainsOutput = false;
  }

  function inspect(event: SubmitEvent) {
    event.preventDefault();
    if (submitted) {
      void preview.refetch();
      return;
    }
    submitted = {
      route_id: routeId,
      api_key_id: keyId,
      operation,
      surface,
      mode: canStream ? delivery : 'unary',
      retains_output: surface === 'openai-responses' && retainsOutput
    };
  }

  const loadError = $derived(routes.error ?? keys.error ?? dialects.error);
</script>

<section class="card stack-card" aria-labelledby="preview-heading">
  <h2 id="preview-heading">Preview guardrails before dispatch</h2>
  <p class="lede">
    See which guardrails apply to a route and API key on the serving release.
    The preview checks the selected surface without sending a request or
    evaluating content.
  </p>
  {#if loadError}
    <Callout tone="danger">{errorMessage(loadError)}</Callout>
  {:else if routes.isPending || keys.isPending || dialects.isPending}
    <p role="status">Loading routes, keys and surfaces…</p>
  {:else if !activeRoutes.length}
    <EmptyState title="No active routes">
      Publish a route before previewing its guardrails.
    </EmptyState>
  {:else}
    <form
      onsubmit={inspect}
      oninput={() => {
        submitted = null;
      }}
    >
      <div class="selection">
        <label class="form-field">
          Route
          <select required bind:value={routeId} onchange={clearRouteSelection}>
            <option value="">Choose an active route</option>
            {#each activeRoutes as item (item.id)}
              <option value={item.id}>{item.slug}</option>
            {/each}
          </select>
        </label>
        <label class="form-field">
          API key
          <select required bind:value={keyId} disabled={!availableKeys.length}>
            <option value="">
              {selectedRoute && !availableKeys.length
                ? 'No keys in this project'
                : 'Choose an API key'}
            </option>
            {#each availableKeys as key (key.id)}
              <option value={key.id}>{key.name} · {key.id.slice(0, 8)}</option>
            {/each}
          </select>
        </label>
        <label class="form-field">
          Operation
          <select
            required
            bind:value={operation}
            disabled={!selectedRoute}
            onchange={clearSurfaceSelection}
          >
            <option value="">Choose an operation</option>
            {#each selectedRoute?.latest_revision.operations ?? [] as item (item)}
              <option value={item}>{item.replaceAll('_', ' ')}</option>
            {/each}
          </select>
        </label>
        <label class="form-field">
          Surface
          <select required bind:value={surface} disabled={!surfaces.length}>
            <option value="">Choose a surface</option>
            {#each surfaces as item (item.id)}
              <option value={item.id}>{item.label}</option>
            {/each}
          </select>
        </label>
        {#if canStream}
          <label class="form-field">
            Delivery mode
            <select bind:value={delivery}>
              <option value="unary">Buffered</option>
              <option value="streaming">Streaming</option>
            </select>
          </label>
        {/if}
      </div>
      {#if surface === 'openai-responses'}
        <label class="check-field">
          <input type="checkbox" bind:checked={retainsOutput} /> Provider retains
          output
        </label>
      {/if}
      <button
        class="button button-primary"
        disabled={!routeId ||
          !keyId ||
          !operation ||
          !surface ||
          preview.isFetching}
      >
        Preview guardrails
      </button>
    </form>
  {/if}
</section>

{#if submitted}
  <section
    class="card stack-card"
    aria-label="Preview result"
    aria-live="polite"
  >
    {#if preview.isFetching}
      <p role="status">Inspecting the serving release…</p>
    {:else if preview.error}
      <Callout tone="danger">{errorMessage(preview.error)}</Callout>
    {:else if preview.data}
      {@const result = preview.data}
      <h2>Serving release {result.release_sequence}</h2>
      <p class="identifier">Release ID <code>{result.release_id}</code></p>
      {#if result.stale}
        <Callout tone="warning">
          A newer release has been published. This preview shows the release
          still serving here.
        </Callout>
      {/if}
      {#if result.partial_visibility}
        <Callout>{partialNotice}</Callout>
      {:else if result.static_compatible === false}
        <Callout tone="warning">
          The guardrail chain has a conflict on the selected surface.
        </Callout>
      {:else if result.static_compatible === true}
        <Callout>
          No static guardrail conflicts found for the selected surface.
        </Callout>
      {/if}
      {#if result.entries.length}
        <ScrollTable label="Guardrails in serving order">
          <table class="data-table">
            <thead>
              <tr>
                <th scope="col">Order</th>
                <th scope="col">Policy and revision</th>
                <th scope="col">Check and revision</th>
                <th scope="col">Scope</th>
                <th scope="col">Phase</th>
                <th scope="col">Mode</th>
                <th scope="col">Compatibility notes</th>
              </tr>
            </thead>
            <tbody>
              {#each result.entries as entry, index (index)}
                <tr>
                  <td>{index + 1}</td>
                  <td class="identifier">
                    <code>{entry.policy_id}</code><br />Revision
                    <code>{entry.policy_revision_id}</code>
                  </td>
                  <td class="identifier">
                    <code>{entry.guardrail_id}</code><br />Revision
                    <code>{entry.revision_id}</code>
                  </td>
                  <td>{entry.scope}</td>
                  <td>{entry.phase.replaceAll('_', ' ')}</td>
                  <td>{entry.mode === 'monitor' ? 'Monitor' : 'Enforce'}</td>
                  <td>
                    {entry.notes.map((note) => notes[note]).join(' ') || 'None'}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </ScrollTable>
      {:else}
        <p>
          {result.partial_visibility
            ? 'No visible guardrails for this selection.'
            : 'No guardrails apply to this selection.'}
        </p>
      {/if}
      <p class="lede">
        Static checks do not verify key permissions, provider support, fallback
        routes or request-specific content.
      </p>
    {/if}
  </section>
{/if}

<style>
  form {
    display: grid;
    gap: 1rem;
  }
  .selection {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 13rem), 1fr));
    gap: 0.75rem 1rem;
  }
  .button {
    justify-self: start;
  }
  .identifier code {
    overflow-wrap: anywhere;
    white-space: normal;
  }
</style>
