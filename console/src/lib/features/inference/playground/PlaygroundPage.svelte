<script lang="ts">
  import RoutingPreferencesForm from '$lib/features/routes/RoutingPreferencesForm.svelte';
  import RoutingDecisions from '$lib/features/routes/RoutingDecisions.svelte';
  import { decisionRows } from '$lib/features/routes/routingExplanation';
  let routing = $state('{}');
  import { routeKeys } from '$lib/features/routes/routeKeys';

  import { createMutation, createQuery } from '@tanstack/svelte-query';
  import {
    listRoutes,
    simulateRouting,
    type RoutingSimulationInput
  } from '$lib/features/routes/api';
  import { hasOutputRules } from '$lib/features/routes/routeEditor';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { apiKeyQueries } from '$lib/features/access/api-keys/apiKeyQueries';
  import {
    runPlayground,
    streamPlayground,
    type PlaygroundOperation,
    type PlaygroundRequest,
    type PlaygroundStreamDone
  } from '$lib/features/inference/playground/api';
  import {
    playgroundTemplates,
    templateFor
  } from '$lib/features/inference/playground/templates';
  import { onDestroy } from 'svelte';
  import SegmentedRadioGroup from '$lib/components/SegmentedRadioGroup.svelte';
  import { abortError, errorMessage } from '$lib/api/http';
  import { formatInteger } from '$lib/format';
  import {
    parseMaxOutputTokens,
    parseResponseSchema,
    parseTemperature,
    parseTools
  } from '$lib/features/inference/playground/validation';

  type Mode = 'text' | 'tools' | 'structured';
  type Composer = 'basic' | 'advanced';
  let mode = $state<Mode>('text');
  let composer = $state<Composer>('basic');
  let operation = $state<PlaygroundOperation>('generation');
  let surface = $state<'openai' | 'anthropic' | 'gemini'>('openai');
  let model = $state('');
  let input = $state('');
  let rawJson = $state(JSON.stringify(playgroundTemplates[0].request, null, 2));
  let templateKey = $state(playgroundTemplates[0].key);
  let streamEnabled = $state(false);
  let streamCheck = $state<
    'idle' | 'checking' | 'ok' | 'unsupported' | 'unknown'
  >('idle');
  let streamCheckMessage = $state('');
  let streaming = $state(false);
  let streamFrames = $state<string[]>([]);
  let streamDone = $state<PlaygroundStreamDone | null>(null);
  let streamProblem = $state<string | null>(null);
  let streamAbort: AbortController | null = null;
  let temperature = $state('');
  let maxOutputTokens = $state('');
  let toolsJson = $state(
    '[\n  {\n    "name": "get_weather",\n    "description": "Get weather for a city",\n    "input_schema": {\n      "type": "object",\n      "properties": { "city": { "type": "string" } },\n      "required": ["city"]\n    }\n  }\n]'
  );
  let schemaJson = $state(
    '{\n  "type": "object",\n  "properties": {\n    "answer": { "type": "string" }\n  },\n  "required": ["answer"],\n  "additionalProperties": false\n}'
  );
  let validationError = $state('');
  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  const apiKeys = createQuery(() => ({
    queryKey: apiKeyQueries.list(),
    queryFn: ({ signal }) => listApiKeys(signal)
  }));
  const mutation = createMutation(() => ({ mutationFn: runPlayground }));
  const selectedRoute = $derived(
    (routes.data ?? []).find((route) => route.slug === model.trim())
  );
  const outputPolicyActive = $derived(
    hasOutputRules(selectedRoute?.latest_revision?.content_policy?.rules ?? [])
  );
  const routeOperations = $derived(
    selectedRoute?.latest_revision?.operations ?? null
  );
  const operationKnown = $derived(
    routeOperations == null || routeOperations.includes(operation)
  );
  const streamSelectable = $derived(
    operation === 'generation' && !outputPolicyActive
  );

  const operations = [
    { value: 'generation', label: 'Generation' },
    { value: 'token_count', label: 'Token count' },
    { value: 'embeddings', label: 'Embeddings' },
    { value: 'moderation', label: 'Moderation' },
    { value: 'rerank', label: 'Rerank' }
  ];
  const composerModes = [
    { value: 'basic', label: 'Basic' },
    { value: 'advanced', label: 'Advanced' }
  ];

  $effect(() => {
    void model;
    void surface;
    void operation;
    streamEnabled = false;
    streamCheck = 'idle';
    streamCheckMessage = '';
  });

  $effect(() => {
    if (
      operation !== 'generation' &&
      operation !== 'token_count' &&
      surface !== 'openai'
    )
      surface = 'openai';
  });

  function applyTemplate(key: string) {
    templateKey = key;
    const template = templateFor(key);
    if (!template) return;
    operation = template.operation;
    if (template.surface) surface = template.surface;
    rawJson = JSON.stringify(template.request, null, 2);
  }

  async function checkStreamCapability() {
    streamCheckMessage = '';
    if (!selectedRoute) {
      streamCheck = 'unknown';
      streamCheckMessage =
        'The route slug is not an active route, so streaming support cannot be verified.';
      return;
    }
    streamCheck = 'checking';
    try {
      const decisions = await simulateRouting({
        route: model.trim(),
        surface,
        mode: 'streaming',
        preferences: JSON.parse(routing || '{}')
      });
      if (decisions.some((decision) => decision.eligible)) {
        streamCheck = 'ok';
      } else {
        streamCheck = 'unsupported';
        streamCheckMessage =
          'No published target reports streaming eligibility for this route and surface.';
      }
    } catch {
      streamCheck = 'unknown';
      streamCheckMessage =
        'Streaming capability could not be verified; the route may reject the stream.';
    }
  }

  function toggleStream(enabled: boolean) {
    streamEnabled = enabled;
    if (enabled) void checkStreamCapability();
    else {
      streamCheck = 'idle';
      streamCheckMessage = '';
    }
  }

  function cancelStream() {
    streamAbort?.abort();
  }

  function clearResult() {
    streamAbort?.abort();
    streamFrames = [];
    streamDone = null;
    streamProblem = null;
    mutation.reset();
  }

  onDestroy(() => {
    streamAbort?.abort();
  });
  const simulation = createMutation(() => ({
    // Wrapped so the mutation context is not passed as the abort signal.
    mutationFn: (input: RoutingSimulationInput) => simulateRouting(input)
  }));

  let simulateKeyId = $state('');
  let simulateSeed = $state('');
  let simulationError = $state('');

  // A dry run is always the most recent action when it has data, because
  // submitting a real test resets it.
  const explanation = $derived(
    simulation.data?.length
      ? { dryRun: true, decisions: simulation.data }
      : mutation.data?.routing?.length
        ? { dryRun: false, decisions: mutation.data.routing }
        : null
  );

  function requestControls() {
    return {
      temperature: parseTemperature(temperature),
      max_output_tokens: parseMaxOutputTokens(maxOutputTokens),
      tools: mode === 'tools' ? parseTools(toolsJson) : undefined,
      response_format:
        mode === 'structured' ? parseResponseSchema(schemaJson) : undefined
    };
  }

  async function explain() {
    simulationError = '';
    if (!model.trim()) {
      simulationError = 'Enter an active route slug.';
      return;
    }
    try {
      await simulation.mutateAsync({
        route: model.trim(),
        surface,
        // Match the unary operation used by the playground endpoint.
        mode: 'unary',
        preferences: JSON.parse(routing),
        ...requestControls(),
        apiKeyId: simulateKeyId || null,
        seed: simulateSeed
      });
    } catch (error) {
      simulationError = errorMessage(
        error,
        'The routing explanation could not be produced.'
      );
    }
  }
  const modes = [
    { value: 'text', label: 'Text' },
    { value: 'tools', label: 'Tools' },
    { value: 'structured', label: 'Structured output' }
  ];

  async function runStream(request: PlaygroundRequest) {
    streamAbort = new AbortController();
    streaming = true;
    streamFrames = [];
    streamDone = null;
    streamProblem = null;
    try {
      await streamPlayground(
        request,
        {
          frame: (frame) => {
            streamFrames = [...streamFrames, frame];
          },
          done: (meta) => {
            streamDone = meta;
          },
          error: (problem) => {
            streamProblem = problem.message ?? 'The playground stream failed.';
          }
        },
        streamAbort.signal
      );
    } catch (error) {
      if (!abortError(error))
        streamProblem = errorMessage(error, 'The playground stream failed.');
    } finally {
      streaming = false;
      streamAbort = null;
    }
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    validationError = '';
    // The run that follows produces its own explanation, so the dry run stops
    // competing for the panel.
    simulation.reset();
    simulationError = '';
    if (!model.trim()) {
      validationError = 'Enter an active route slug.';
      return;
    }
    if (composer === 'advanced' && !operationKnown) {
      validationError = `The route does not offer the ${operation} operation.`;
      return;
    }
    if (streamEnabled && streamCheck !== 'ok') {
      validationError =
        'Streaming has not been verified as available for this route.';
      return;
    }
    let request: PlaygroundRequest;
    try {
      if (composer === 'advanced') {
        const raw: unknown = JSON.parse(rawJson);
        if (!raw || typeof raw !== 'object' || Array.isArray(raw)) {
          validationError = 'The request document must be a JSON object.';
          return;
        }
        request = {
          routing: JSON.parse(routing),
          model: model.trim(),
          surface,
          operation,
          request: raw as Record<string, unknown>,
          stream: streamEnabled ? true : undefined
        };
      } else {
        if (!input.trim()) {
          validationError = 'Enter a prompt.';
          return;
        }
        request = {
          routing: JSON.parse(routing),
          model: model.trim(),
          input,
          surface,
          stream: streamEnabled ? true : undefined,
          ...requestControls()
        };
      }
    } catch (error) {
      validationError = errorMessage(error, 'Check the request fields.');
      return;
    }
    streamFrames = [];
    streamDone = null;
    streamProblem = null;
    if (streamEnabled) {
      await runStream(request);
      return;
    }
    // A transport or route failure is rendered by the result panel; rethrowing
    // here would leave an unhandled rejection with nothing to catch it.
    try {
      await mutation.mutateAsync(request);
    } catch {
      validationError = '';
    }
  }
</script>

<svelte:head><title>Playground · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Tools</p>
    <h1 class="page-title">Playground</h1>
    <p class="page-description">
      Test an active route with your signed-in session. No saved proxy key is
      required, and prompt or output content is not persisted.
    </p>
  </div>
</div>

<div class="privacy-note">
  <span aria-hidden="true">◉</span>
  <p>
    <strong>Ephemeral content</strong><br />Input and output exist only for this
    browser request. Operational request history is not created by playground
    runs.
  </p>
</div>

<div class="playground-grid">
  <form class="card composer" onsubmit={submit}>
    <RoutingPreferencesForm
      bind:value={routing}
      id="playground-routing"
      disabled={mutation.isPending || streaming}
    />
    <SegmentedRadioGroup
      label="Composer"
      name="playground-composer"
      value={composer}
      items={composerModes}
      onChange={(value) => {
        if (value === 'basic' || value === 'advanced') composer = value;
      }}
    />
    {#if composer === 'advanced'}
      <div class="route-grid">
        <div class="form-field">
          <label for="playground-operation">Operation</label><select
            id="playground-operation"
            bind:value={operation}
          >
            {#each operations as option (option.value)}<option
                value={option.value}>{option.label}</option
              >{/each}
          </select>
          {#if !operationKnown}<small class="field-error" role="alert"
              >The selected route does not offer this operation.</small
            >{:else if routeOperations == null}<small
              class="policy-note"
              role="status"
              >Route capabilities are unknown until a matching active route is
              entered — the route enforces the final decision.</small
            >{/if}
        </div>
        <div class="form-field">
          <label for="playground-template">Template</label><select
            id="playground-template"
            value={templateKey}
            onchange={(event) => applyTemplate(event.currentTarget.value)}
          >
            {#each playgroundTemplates as template (template.key)}<option
                value={template.key}>{template.label}</option
              >{/each}
          </select><small
            >Loads a request document; tools are never executed.</small
          >
        </div>
      </div>
    {:else}
      <SegmentedRadioGroup
        label="Test mode"
        name="playground-mode"
        value={mode}
        items={modes}
        onChange={(value) => {
          if (value === 'text' || value === 'tools' || value === 'structured')
            mode = value;
        }}
      />
    {/if}
    <div class="route-grid">
      <div class="form-field">
        <label for="playground-model">Route slug</label><input
          id="playground-model"
          bind:value={model}
          list="playground-routes"
          autocomplete="off"
          placeholder="support-chat"
          aria-describedby="model-help route-status"
        />
        <datalist id="playground-routes">
          {#each routes.data ?? [] as route (route.id)}
            <option value={route.slug}></option>
          {/each}
        </datalist>
        <small id="model-help"
          >Choose an active route suggestion or enter its public slug.</small
        >
        {#if outputPolicyActive}<small class="policy-note" role="status"
            >This route enforces an output content policy — streaming requests
            are rejected. Playground runs are unary and still permitted.</small
          >{/if}
        <div id="route-status">
          {#if routes.isPending}
            <small role="status">Loading active routes…</small>
          {:else if routes.isError}
            <p class="field-error" role="alert">
              {errorMessage(routes.error, 'Active routes could not be loaded.')}
            </p>
            <small>You can still enter a route slug.</small>
            <button
              type="button"
              class="button button-secondary"
              onclick={() => routes.refetch()}
              disabled={routes.isFetching}>Retry routes</button
            >
          {:else if routes.data?.length === 0}
            <small
              >No active routes are available. Activate a route to get started.</small
            >
          {/if}
        </div>
      </div>
      <div class="form-field">
        <label for="playground-surface">Client surface</label><select
          id="playground-surface"
          bind:value={surface}
          disabled={composer === 'advanced' &&
            operation !== 'generation' &&
            operation !== 'token_count'}
          ><option value="openai">OpenAI</option><option value="anthropic"
            >Anthropic</option
          ><option value="gemini">Gemini</option></select
        ><small
          >{composer === 'advanced' &&
          operation !== 'generation' &&
          operation !== 'token_count'
            ? 'This operation is served on the OpenAI surface.'
            : 'Capability filtering uses this originating protocol.'}</small
        >
      </div>
    </div>
    {#if composer === 'basic'}
      <div class="route-grid">
        <div class="form-field">
          <label for="playground-temperature">Temperature</label><input
            id="playground-temperature"
            bind:value={temperature}
            inputmode="decimal"
            autocomplete="off"
            placeholder="Provider default"
            aria-describedby="temperature-help"
          /><small id="temperature-help">0 through 2.</small>
        </div>
        <div class="form-field">
          <label for="playground-max-output">Max output tokens</label><input
            id="playground-max-output"
            bind:value={maxOutputTokens}
            inputmode="numeric"
            autocomplete="off"
            placeholder="Provider default"
            aria-describedby="max-output-help"
          /><small id="max-output-help">1 through 1000000.</small>
        </div>
      </div>
      <div class="form-field">
        <label for="playground-input">Prompt</label><textarea
          id="playground-input"
          bind:value={input}
          rows="9"
          placeholder="Ask the model something…"></textarea>
      </div>
      {#if mode === 'tools'}<div class="form-field">
          <label for="playground-tools">Tools JSON</label><textarea
            id="playground-tools"
            bind:value={toolsJson}
            rows="12"
            class="mono"
            spellcheck="false"></textarea>
        </div>{/if}
      {#if mode === 'structured'}<div class="form-field">
          <label for="playground-schema">JSON Schema</label><textarea
            id="playground-schema"
            bind:value={schemaJson}
            rows="12"
            class="mono"
            spellcheck="false"></textarea>
        </div>{/if}
    {:else}
      <div class="form-field">
        <label for="playground-raw">Request JSON</label><textarea
          id="playground-raw"
          bind:value={rawJson}
          rows="16"
          class="mono"
          spellcheck="false"
          aria-describedby="raw-help"></textarea>
        <small id="raw-help"
          >Raw public request body. The route slug above replaces its model
          field — provider addresses cannot be set here.</small
        >
      </div>
    {/if}
    {#if operation === 'generation'}
      <div class="form-field stream-toggle">
        <label class="checkbox-label"
          ><input
            type="checkbox"
            checked={streamEnabled}
            disabled={!streamSelectable || streaming}
            onchange={(event) => toggleStream(event.currentTarget.checked)}
          />
          Stream the response</label
        >
        {#if outputPolicyActive}<small class="policy-note" role="status"
            >Output content policy requires unary responses — streaming is
            disabled.</small
          >{:else if streamEnabled && streamCheck === 'checking'}<small
            role="status">Checking streaming eligibility…</small
          >{:else if streamEnabled && streamCheckMessage}<small
            class="policy-note"
            role="status">{streamCheckMessage}</small
          >{/if}
      </div>
    {/if}
    {#if validationError}<p class="field-error" role="alert">
        {validationError}
      </p>{/if}
    <details class="dry-run">
      <summary>Explain routing without running</summary>
      <p class="dry-run-help">
        Ranks the attempts the published runtime would make for a generation on
        this route, honouring circuit-breaker state, without sending a request
        to any provider. Nothing is billed and no prompt is needed.
      </p>
      <div class="route-grid">
        <div class="form-field">
          <label for="playground-simulate-key">Evaluate as API key</label
          ><select
            id="playground-simulate-key"
            bind:value={simulateKeyId}
            aria-describedby="simulate-key-help"
          >
            <option value="">No key restriction</option>
            {#each apiKeys.data ?? [] as key (key.id)}
              {#if !key.revoked_at}<option value={key.id}>{key.name}</option
                >{/if}
            {/each}
          </select><small id="simulate-key-help"
            >Applies that key's route allowlist and scopes to the explanation.</small
          >
        </div>
        <div class="form-field">
          <label for="playground-simulate-seed">Seed</label><input
            id="playground-simulate-seed"
            bind:value={simulateSeed}
            autocomplete="off"
            maxlength="256"
            placeholder="Any stable value"
            aria-describedby="simulate-seed-help"
          /><small id="simulate-seed-help"
            >Fixes weighted tie-breaks so the order is reproducible.</small
          >
        </div>
      </div>
      {#if simulationError}<p class="field-error" role="alert">
          {simulationError}
        </p>{/if}
      <button
        class="button button-secondary"
        type="button"
        disabled={simulation.isPending}
        onclick={explain}
        >{simulation.isPending ? 'Explaining…' : 'Explain routing'}</button
      >
    </details>
    <div class="run-actions">
      <button
        class="button button-primary"
        type="submit"
        disabled={mutation.isPending ||
          streaming ||
          (composer === 'advanced' && !operationKnown)}
        >{mutation.isPending || streaming ? 'Running…' : 'Run test'}</button
      >
      {#if streaming}<button
          class="button button-secondary"
          type="button"
          onclick={cancelStream}>Cancel</button
        >{/if}
      {#if streamFrames.length > 0 || streamDone || streamProblem || mutation.data}<button
          class="button button-secondary"
          type="button"
          onclick={clearResult}>Clear</button
        >{/if}
    </div>
  </form>

  <section
    class="card result"
    aria-labelledby="result-title"
    aria-live="polite"
  >
    <div class="result-heading">
      <div>
        <p class="eyebrow">Ephemeral response</p>
        <h2 id="result-title">Result</h2>
      </div>
      {#if mutation.data && !mutation.isPending}<span class="badge success"
          >{mutation.data.latency_ms} ms</span
        >{/if}
    </div>
    {#if streaming || streamFrames.length > 0 || streamDone || streamProblem}
      {#if streamProblem}<div class="inline-problem" role="alert">
          {streamProblem}
        </div>{/if}
      {#if streamFrames.length > 0}<div class="output">
          <h3>Frames</h3>
          <pre data-testid="stream-frames">{streamFrames.join('')}</pre>
        </div>{/if}
      {#if streaming}<div class="loading-state" role="status">
          Streaming response…
        </div>{/if}
      {#if streamDone}<dl>
          <div>
            <dt>Response ID</dt>
            <dd class="mono">{streamDone.id ?? 'Not reported'}</dd>
          </div>
          <div>
            <dt>Route</dt>
            <dd>{streamDone.model ?? 'Not reported'}</dd>
          </div>
          <div>
            <dt>Input tokens</dt>
            <dd>{formatInteger(streamDone.usage?.input_tokens)}</dd>
          </div>
          <div>
            <dt>Output tokens</dt>
            <dd>{formatInteger(streamDone.usage?.output_tokens)}</dd>
          </div>
          <div>
            <dt>Total tokens</dt>
            <dd>{formatInteger(streamDone.usage?.total_tokens)}</dd>
          </div>
        </dl>{/if}
    {:else if mutation.isPending}<div class="loading-state" role="status">
        Waiting for the route…
      </div>
    {:else if mutation.isError}<div class="inline-problem" role="alert">
        {errorMessage(mutation.error, 'The playground request failed.')}
      </div>
    {:else if mutation.data}
      {#if mutation.data.refusal}<div class="refusal" role="alert">
          <strong>The model refused this request</strong>
          <p>{mutation.data.refusal}</p>
        </div>{/if}
      {#if !mutation.data.refusal && !mutation.data.output_text && !mutation.data.tool_calls?.length && mutation.data.response === undefined && (mutation.data.structured_output === undefined || mutation.data.structured_output === null)}<div
          class="empty-state"
        >
          <div>
            <strong>No content returned</strong>
            <p>
              The route answered without text, a tool call, or structured
              output. The finish reason below says why.
            </p>
          </div>
        </div>{/if}
      {#if mutation.data.response !== undefined}<div class="output">
          <h3>Operation result</h3>
          <pre>{JSON.stringify(mutation.data.response, null, 2)}</pre>
        </div>{/if}
      {#if mutation.data.output_text}<div class="output">
          <h3>Text</h3>
          <pre>{mutation.data.output_text}</pre>
        </div>{/if}
      {#if mutation.data.tool_calls?.length}<div class="output">
          <h3>Tool calls</h3>
          <pre>{JSON.stringify(mutation.data.tool_calls, null, 2)}</pre>
        </div>{/if}
      {#if mutation.data.structured_output !== undefined && mutation.data.structured_output !== null}<div
          class="output"
        >
          <h3>Structured output</h3>
          <pre>{JSON.stringify(mutation.data.structured_output, null, 2)}</pre>
        </div>{/if}
      <dl>
        <div>
          <dt>Response ID</dt>
          <dd class="mono">{mutation.data.id}</dd>
        </div>
        <div>
          <dt>Route</dt>
          <dd>{mutation.data.model}</dd>
        </div>
        <div>
          <dt>Provider model</dt>
          <dd>{mutation.data.provider_model ?? 'Not reported'}</dd>
        </div>
        <div>
          <dt>Finish reason</dt>
          <dd>{mutation.data.finish_reason ?? 'Not reported'}</dd>
        </div>
        <div>
          <dt>Input tokens</dt>
          <dd>{formatInteger(mutation.data.usage?.input_tokens)}</dd>
        </div>
        <div>
          <dt>Cached input tokens</dt>
          <dd>{formatInteger(mutation.data.usage?.cached_input_tokens)}</dd>
        </div>
        <div>
          <dt>Output tokens</dt>
          <dd>{formatInteger(mutation.data.usage?.output_tokens)}</dd>
        </div>
        <div>
          <dt>Reasoning tokens</dt>
          <dd>{formatInteger(mutation.data.usage?.reasoning_tokens)}</dd>
        </div>
        <div>
          <dt>Total tokens</dt>
          <dd>{formatInteger(mutation.data.usage?.total_tokens)}</dd>
        </div>
      </dl>
    {:else}<div class="empty-state">
        <div>
          <strong>Ready to test</strong>
          <p>
            Choose a mode, enter an active route slug, and run an ephemeral
            request.
          </p>
        </div>
      </div>{/if}
  </section>
</div>

{#if explanation}<section class="card composer">
    <h2>Routing explanation</h2>
    <p class="explanation-source">
      {explanation.dryRun
        ? 'Dry run against the published runtime. No provider request was sent.'
        : 'From the request that just ran.'}
    </p>
    <RoutingDecisions rows={decisionRows(explanation.decisions)} />
  </section>{/if}

<style>
  .explanation-source {
    margin: 0 0 1rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .dry-run {
    margin-top: 1rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border-hairline);
  }
  .dry-run summary {
    cursor: pointer;
  }
  .dry-run-help {
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .privacy-note {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    margin-top: 1.25rem;
    padding: 0.85rem 1rem;
    border: 1px solid var(--success);
    border-radius: var(--radius-control);
    background: var(--success-soft);
    color: var(--foreground);
    line-height: 1.5;
  }
  .privacy-note span,
  .privacy-note strong {
    color: var(--success);
  }
  .privacy-note p {
    margin: 0;
  }
  .playground-grid {
    display: grid;
    grid-template-columns: minmax(22rem, 0.9fr) minmax(24rem, 1.1fr);
    gap: 1rem;
    margin-top: 1rem;
    align-items: start;
  }
  .composer,
  .result {
    padding: 1.5rem;
  }
  .composer {
    display: grid;
    gap: 1rem;
  }
  .route-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.8rem;
  }
  .field-error {
    margin: 0;
    color: var(--danger);
  }
  .run-actions {
    display: flex;
    gap: 0.5rem;
  }
  .stream-toggle {
    margin: 0;
  }
  .checkbox-label {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 500;
  }
  .policy-note {
    margin-top: 0.35rem;
    color: var(--warning);
  }
  .form-field small {
    display: block;
    margin-top: 0.25rem;
    font-size: var(--text-caption);
  }
  .refusal {
    margin-top: 1rem;
    padding: 0.85rem 1rem;
    border: 1px solid var(--warning);
    border-radius: var(--radius-control);
    background: var(--warning-soft);
    color: var(--foreground);
  }
  .refusal strong {
    color: var(--warning);
  }
  .refusal p {
    margin: 0.25rem 0 0;
  }
  .result-heading {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  h3 {
    margin: 0 0 0.5rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .output {
    margin-top: 1rem;
  }
  pre {
    max-height: 28rem;
    margin: 0;
    overflow: auto;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    line-height: 1.5;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.75rem;
    margin: 1rem 0 0;
    padding-top: 1rem;
    border-top: 1px solid var(--border-hairline);
  }
  dt {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  dd {
    margin: 0.25rem 0 0;
    overflow-wrap: anywhere;
  }
  @media (max-width: 68rem) {
    .playground-grid {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 38rem) {
    .composer,
    .result {
      padding: 1rem;
    }
    .route-grid {
      display: grid;
      grid-template-columns: 1fr;
    }
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
