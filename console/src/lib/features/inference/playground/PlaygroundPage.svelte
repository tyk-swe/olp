<script lang="ts">
  import RoutingPreferencesForm from '$lib/features/routes/RoutingPreferencesForm.svelte';
  import RoutingDecisions from '$lib/features/routes/RoutingDecisions.svelte';
  import { decisionRows } from '$lib/features/routes/routingExplanation';
  import { inspectionDialects } from '$lib/features/routes/inspectionDialects';
  import SegmentedRadioGroup from '$lib/components/SegmentedRadioGroup.svelte';
  import { errorMessage } from '$lib/api/http';
  import { formatInteger } from '$lib/format';
  import RealtimeTrace from './RealtimeTrace.svelte';
  import OperationResult from './OperationResult.svelte';
  import StrictToolPlayground from './StrictToolPlayground.svelte';
  import NativeOperationPlayground from './NativeOperationPlayground.svelte';
  import AudioTranslationPlayground from './AudioTranslationPlayground.svelte';
  import { nativeDialects, type NativeOperation } from './nativeOperation';
  import { playgroundTemplates } from './templates';
  import { PlaygroundState } from './playgroundState.svelte';

  const playground = new PlaygroundState();
  const operations = [
    { value: 'generation', label: 'Generation' },
    { value: 'token_count', label: 'Token count' },
    { value: 'embeddings', label: 'Embeddings' },
    { value: 'moderation', label: 'Moderation' },
    { value: 'rerank', label: 'Rerank' },
    { value: 'classification', label: 'Classification' },
    { value: 'scoring', label: 'Scoring' },
    { value: 'translation', label: 'Audio translation' },
    { value: 'realtime', label: 'Realtime event trace' }
  ];

  const composerModes = [
    { value: 'basic', label: 'Basic' },
    { value: 'advanced', label: 'Advanced' }
  ];

  const modes = [
    { value: 'text', label: 'Text' },
    { value: 'tools', label: 'Tools' },
    { value: 'structured', label: 'Structured output' }
  ];
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
  <form class="card composer" onsubmit={playground.submit}>
    <RoutingPreferencesForm
      bind:value={playground.routing}
      id="playground-routing"
      disabled={playground.mutation.isPending || playground.streaming}
    />
    <SegmentedRadioGroup
      label="Composer"
      name="playground-composer"
      value={playground.composer}
      items={composerModes}
      onChange={(value) => {
        if (value === 'basic' || value === 'advanced')
          playground.composer = value;
      }}
    />
    {#if playground.composer === 'advanced'}
      <div class="route-grid">
        <div class="form-field">
          <label for="playground-operation">Operation</label><select
            id="playground-operation"
            bind:value={playground.operation}
          >
            {#each operations as option (option.value)}<option
                value={option.value}>{option.label}</option
              >{/each}
          </select>
          {#if !playground.operationKnown}<small
              class="field-error"
              role="alert"
              >The selected route does not offer this operation.</small
            >{:else if playground.routeOperations == null}<small
              class="policy-note"
              role="status"
              >Route capabilities are unknown until a matching active route is
              entered — the route enforces the final decision.</small
            >{/if}
        </div>
        {#if playground.operation === 'realtime'}
          <p class="policy-note">
            The local trace viewer below does not use a route or request
            template and starts no session.
          </p>
        {:else}
          <div class="form-field">
            <label for="playground-template">Template</label><select
              id="playground-template"
              value={playground.templateKey}
              onchange={(event) =>
                playground.applyTemplate(event.currentTarget.value)}
            >
              {#each playgroundTemplates as template (template.key)}<option
                  value={template.key}>{template.label}</option
                >{/each}
            </select><small
              >Loads a request document; tools are never executed.</small
            >
          </div>
        {/if}
      </div>
    {:else}
      <SegmentedRadioGroup
        label="Test mode"
        name="playground-mode"
        value={playground.mode}
        items={modes}
        onChange={(value) => {
          if (value === 'text' || value === 'tools' || value === 'structured')
            playground.mode = value;
        }}
      />
    {/if}
    <div class="route-grid">
      <div class="form-field">
        <label for="playground-model">Route slug</label><input
          id="playground-model"
          bind:value={playground.model}
          list="playground-routes"
          autocomplete="off"
          placeholder="support-chat"
          aria-describedby="model-help route-status"
        />
        <datalist id="playground-routes">
          {#each playground.routes.data ?? [] as route (route.id)}
            <option value={route.slug}></option>
          {/each}
        </datalist>
        <small id="model-help"
          >Choose an active route suggestion or enter its public slug.</small
        >
        {#if playground.outputPolicyActive}<small
            class="policy-note"
            role="status"
            >This route enforces an output content policy — streaming requests
            are rejected. Playground runs are unary and still permitted.</small
          >{/if}
        <div id="route-status">
          {#if playground.routes.isPending}
            <small class="inline-status" role="status"
              >Loading active routes…</small
            >
          {:else if playground.routes.isError}
            <p class="field-error" role="alert">
              {errorMessage(
                playground.routes.error,
                'Active routes could not be loaded.'
              )}
            </p>
            <small>You can still enter a route slug.</small>
            <button
              type="button"
              class="button button-secondary"
              onclick={() => playground.routes.refetch()}
              disabled={playground.routes.isFetching}>Retry routes</button
            >
          {:else if playground.routes.data?.length === 0}
            <small
              >No active routes are available. Activate a route to get started.</small
            >
          {/if}
        </div>
      </div>
      <div class="form-field">
        <label for="playground-surface">Client surface</label><select
          id="playground-surface"
          bind:value={playground.surface}
          disabled={playground.composer === 'advanced' &&
            playground.operation !== 'generation' &&
            playground.operation !== 'token_count'}
          ><option value="openai">OpenAI</option><option value="anthropic"
            >Anthropic</option
          ><option value="gemini">Gemini</option></select
        ><small
          >{playground.composer === 'advanced' &&
          playground.operation !== 'generation' &&
          playground.operation !== 'token_count'
            ? 'This operation is served on the OpenAI surface.'
            : 'Capability filtering uses this originating protocol.'}</small
        >
      </div>
    </div>
    {#if playground.composer === 'basic'}
      <div class="route-grid">
        <div class="form-field">
          <label for="playground-temperature">Temperature</label><input
            id="playground-temperature"
            bind:value={playground.temperature}
            inputmode="decimal"
            autocomplete="off"
            placeholder="Provider default"
            aria-describedby="temperature-help"
          /><small id="temperature-help">0 through 2.</small>
        </div>
        <div class="form-field">
          <label for="playground-max-output">Max output tokens</label><input
            id="playground-max-output"
            bind:value={playground.maxOutputTokens}
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
          bind:value={playground.input}
          rows="9"
          placeholder="Ask the model something…"></textarea>
      </div>
      {#if playground.mode === 'tools'}<div class="form-field">
          <label for="playground-tools">Tools JSON</label><textarea
            id="playground-tools"
            bind:value={playground.toolsJson}
            rows="12"
            class="mono"
            spellcheck="false"></textarea>
        </div>{/if}
      {#if playground.mode === 'structured'}<div class="form-field">
          <label for="playground-schema">JSON Schema</label><textarea
            id="playground-schema"
            bind:value={playground.schemaJson}
            rows="12"
            class="mono"
            spellcheck="false"></textarea>
        </div>{/if}
    {:else if playground.operation !== 'realtime' && playground.operation !== 'translation'}
      <div class="form-field">
        <label for="playground-raw">Request JSON</label><textarea
          id="playground-raw"
          bind:value={playground.rawJson}
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
    {#if playground.activeOperation === 'generation'}
      <div class="form-field stream-toggle">
        <label class="checkbox-label"
          ><input
            type="checkbox"
            checked={playground.streamEnabled}
            disabled={!playground.streamSelectable || playground.streaming}
            onchange={(event) =>
              playground.toggleStream(event.currentTarget.checked)}
          />
          Stream the response</label
        >
        {#if playground.outputPolicyActive}<small
            class="policy-note"
            role="status"
            >Output content policy requires unary responses — streaming is
            disabled.</small
          >{:else if playground.streamEnabled && playground.streamCheck === 'checking'}<small
            class="inline-status"
            role="status">Checking streaming eligibility…</small
          >{:else if playground.streamEnabled && playground.streamCheckMessage}<small
            class="policy-note"
            role="status">{playground.streamCheckMessage}</small
          >{/if}
      </div>
    {/if}
    {#if playground.validationError}<p class="field-error" role="alert">
        {playground.validationError}
      </p>{/if}
    {#if playground.strictSelected && playground.activeOperation !== 'realtime'}<p
        class="policy-note"
        role="status"
      >
        This route requires a client that retains its native observation and
        continuation contract. Use the public strict client below with an
        authorized inference key.
      </p>{/if}
    <details class="dry-run">
      <summary>Inspect effective plan without running</summary>
      <p class="dry-run-help">
        Shows current target eligibility and, in Advanced mode, the native
        request the interaction planner would prepare. Basic mode checks target
        eligibility only. No provider inference, job, or tool is started, and
        nothing is billed.
      </p>
      {#if playground.composer === 'advanced'}
        <div class="form-field">
          <label for="playground-inspect-dialect">Native request dialect</label>
          <select
            id="playground-inspect-dialect"
            value={playground.strictSelected &&
            nativeDialects(playground.operation).length
              ? playground.currentNativeDialect()
              : (playground.currentInspectDialect() ?? '')}
            onchange={(event) => {
              if (
                playground.strictSelected &&
                nativeDialects(playground.operation).length
              )
                playground.nativeDialect = event.currentTarget.value;
              else playground.inspectDialect = event.currentTarget.value;
            }}
          >
            <option value="">Default for operation and surface</option>
            {#each playground.strictSelected && nativeDialects(playground.operation).length ? nativeDialects(playground.operation) : inspectionDialects(playground.operation, playground.surface) as dialect (dialect)}
              <option value={dialect}>{dialect}</option>
            {/each}
          </select>
          <small
            >The request JSON above must name this route when its native dialect
            carries a model field.</small
          >
        </div>
      {/if}
      <div class="route-grid">
        <div class="form-field">
          <label for="playground-simulate-key">Evaluate as API key</label
          ><select
            id="playground-simulate-key"
            bind:value={playground.simulateKeyId}
            aria-describedby="simulate-key-help"
          >
            <option value="">No API key authority</option>
            {#each playground.apiKeys.data ?? [] as key (key.id)}
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
            bind:value={playground.simulateSeed}
            autocomplete="off"
            maxlength="256"
            placeholder="Any stable value"
            aria-describedby="simulate-seed-help"
            onkeydown={(event) => {
              // Enter would implicitly submit the composer and start a real run.
              if (event.key !== 'Enter' || event.isComposing) return;
              event.preventDefault();
              if (!playground.simulation.isPending) void playground.explain();
            }}
          /><small id="simulate-seed-help"
            >Fixes weighted tie-breaks so the order is reproducible.</small
          >
        </div>
      </div>
      {#if playground.simulationError}<p class="field-error" role="alert">
          {playground.simulationError}
        </p>{/if}
      <button
        class="button button-secondary"
        type="button"
        disabled={playground.simulation.isPending}
        onclick={playground.explain}
        >{playground.simulation.isPending
          ? 'Inspecting…'
          : 'Inspect plan'}</button
      >
    </details>
    <div class="run-actions">
      <button
        class="button button-primary"
        type="submit"
        disabled={playground.mutation.isPending ||
          playground.streaming ||
          playground.strictSelected ||
          playground.activeOperation === 'realtime' ||
          playground.activeOperation === 'translation' ||
          (playground.composer === 'advanced' && !playground.operationKnown)}
        >{playground.mutation.isPending || playground.streaming
          ? 'Running…'
          : 'Run test'}</button
      >
      {#if playground.streaming}<button
          class="button button-secondary"
          type="button"
          onclick={playground.cancelStream}>Cancel</button
        >{/if}
      {#if playground.streamFrames.length > 0 || playground.streamDone || playground.streamProblem || playground.mutation.data}<button
          class="button button-secondary"
          type="button"
          onclick={playground.clearResult}>Clear</button
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
      {#if playground.mutation.data && !playground.mutation.isPending}<span
          class="badge success">{playground.mutation.data.latency_ms} ms</span
        >{/if}
    </div>
    {#if playground.streaming || playground.streamFrames.length > 0 || playground.streamDone || playground.streamProblem}
      {#if playground.streamProblem}<div class="inline-problem" role="alert">
          {playground.streamProblem}
        </div>{/if}
      {#if playground.streamFrames.length > 0}<div class="output">
          <h3>Frames</h3>
          <pre data-testid="stream-frames">{playground.streamFrames.join(
              ''
            )}</pre>
        </div>{/if}
      {#if playground.streaming}<div class="loading-state" role="status">
          Streaming response…
        </div>{/if}
      {#if playground.streamDone}<dl>
          <div>
            <dt>Response ID</dt>
            <dd class="mono">{playground.streamDone.id ?? 'Not reported'}</dd>
          </div>
          <div>
            <dt>Route</dt>
            <dd>{playground.streamDone.model ?? 'Not reported'}</dd>
          </div>
          <div>
            <dt>Input tokens</dt>
            <dd>{formatInteger(playground.streamDone.usage?.input_tokens)}</dd>
          </div>
          <div>
            <dt>Output tokens</dt>
            <dd>{formatInteger(playground.streamDone.usage?.output_tokens)}</dd>
          </div>
          <div>
            <dt>Total tokens</dt>
            <dd>{formatInteger(playground.streamDone.usage?.total_tokens)}</dd>
          </div>
        </dl>{/if}
    {:else if playground.mutation.isPending}<div
        class="loading-state"
        role="status"
      >
        Waiting for the route…
      </div>
    {:else if playground.mutation.isError}<div
        class="inline-problem"
        role="alert"
      >
        {errorMessage(
          playground.mutation.error,
          'The playground request failed.'
        )}
      </div>
    {:else if playground.mutation.data}
      {#if playground.mutation.data.refusal}<div class="refusal" role="alert">
          <strong>The model refused this request</strong>
          <p>{playground.mutation.data.refusal}</p>
        </div>{/if}
      {#if !playground.mutation.data.refusal && !playground.mutation.data.output_text && !playground.mutation.data.tool_calls?.length && playground.mutation.data.response === undefined && (playground.mutation.data.structured_output === undefined || playground.mutation.data.structured_output === null)}<div
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
      {#if playground.mutation.data.response !== undefined}<div class="output">
          <OperationResult
            operation={playground.completedRequest?.operation ?? 'generation'}
            response={playground.mutation.data.response}
            responseRaw={playground.mutation.data.response_raw}
            request={playground.completedRequest?.request}
          />
        </div>{/if}
      {#if playground.mutation.data.output_text}<div class="output">
          <h3>Text</h3>
          <pre>{playground.mutation.data.output_text}</pre>
        </div>{/if}
      {#if playground.mutation.data.tool_calls?.length}<div class="output">
          <h3>Tool calls</h3>
          <pre>{JSON.stringify(
              playground.mutation.data.tool_calls,
              null,
              2
            )}</pre>
        </div>{/if}
      {#if playground.mutation.data.structured_output !== undefined && playground.mutation.data.structured_output !== null}<div
          class="output"
        >
          <h3>Structured output</h3>
          <pre>{JSON.stringify(
              playground.mutation.data.structured_output,
              null,
              2
            )}</pre>
        </div>{/if}
      <dl>
        <div>
          <dt>Response ID</dt>
          <dd class="mono">{playground.mutation.data.id}</dd>
        </div>
        <div>
          <dt>Route</dt>
          <dd>{playground.mutation.data.model}</dd>
        </div>
        <div>
          <dt>Provider model</dt>
          <dd>{playground.mutation.data.provider_model ?? 'Not reported'}</dd>
        </div>
        <div>
          <dt>Finish reason</dt>
          <dd>{playground.mutation.data.finish_reason ?? 'Not reported'}</dd>
        </div>
        <div>
          <dt>Input tokens</dt>
          <dd>{formatInteger(playground.mutation.data.usage?.input_tokens)}</dd>
        </div>
        <div>
          <dt>Cached input tokens</dt>
          <dd>
            {formatInteger(playground.mutation.data.usage?.cached_input_tokens)}
          </dd>
        </div>
        <div>
          <dt>Output tokens</dt>
          <dd>
            {formatInteger(playground.mutation.data.usage?.output_tokens)}
          </dd>
        </div>
        <div>
          <dt>Reasoning tokens</dt>
          <dd>
            {formatInteger(playground.mutation.data.usage?.reasoning_tokens)}
          </dd>
        </div>
        <div>
          <dt>Total tokens</dt>
          <dd>{formatInteger(playground.mutation.data.usage?.total_tokens)}</dd>
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

{#if playground.strictSelected && playground.activeOperation !== 'translation'}
  {#if playground.composer === 'advanced' && playground.operation === 'generation' && playground.surface === 'openai'}
    {#key playground.model.trim()}
      <StrictToolPlayground
        route={playground.model.trim()}
        requestText={playground.rawJson}
      />
    {/key}
  {:else if playground.composer === 'advanced' && nativeDialects(playground.operation).length}
    {#key `${playground.model.trim()}:${playground.operation}:${playground.templateKey}`}
      <NativeOperationPlayground
        route={playground.model.trim()}
        operation={playground.operation as NativeOperation}
        requestText={playground.rawJson}
        bind:dialect={playground.nativeDialect}
      />
    {/key}
  {:else if playground.activeOperation !== 'realtime'}
    <section class="card strict-client-unavailable" role="status">
      This route needs a native or negotiated public client. Choose Advanced,
      Generation, OpenAI and the negotiated tool template for the currently
      qualified tool workflow.
    </section>
  {/if}
{/if}

{#if playground.composer === 'advanced' && playground.operation === 'translation'}
  {#key playground.model.trim()}<AudioTranslationPlayground
      route={playground.model.trim()}
    />{/key}
{/if}

{#if playground.composer === 'advanced' && playground.operation === 'realtime'}
  <RealtimeTrace />
{/if}

{#if playground.explanation}<section class="card composer">
    <h2>Routing explanation</h2>
    <p class="explanation-source">
      {playground.explanation.dryRun
        ? 'Dry run against the published runtime. No provider request was sent.'
        : 'From the request that just ran.'}
    </p>
    <RoutingDecisions rows={decisionRows(playground.explanation.decisions)} />
  </section>{/if}

<style>
  .strict-client-unavailable {
    padding: 1.25rem;
    margin-top: 1rem;
  }
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
