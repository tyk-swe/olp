<script lang="ts">
  import { createMutation } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { runPlayground, type PlaygroundRequest } from '$lib/api/playground';
  import SegmentedRadioGroup from '$lib/components/SegmentedRadioGroup.svelte';
  import { formatInteger } from '$lib/format';
  import {
    parseMaxOutputTokens,
    parseResponseSchema,
    parseTemperature,
    parseTools
  } from './validation';

  type Mode = 'text' | 'tools' | 'structured';
  let mode = $state<Mode>('text');
  let surface = $state<'openai' | 'anthropic' | 'gemini'>('openai');
  let model = $state('');
  let input = $state('');
  let temperature = $state('');
  let maxOutputTokens = $state('');
  let toolsJson = $state(
    '[\n  {\n    "name": "get_weather",\n    "description": "Get weather for a city",\n    "input_schema": {\n      "type": "object",\n      "properties": { "city": { "type": "string" } },\n      "required": ["city"]\n    }\n  }\n]'
  );
  let schemaJson = $state(
    '{\n  "type": "object",\n  "properties": {\n    "answer": { "type": "string" }\n  },\n  "required": ["answer"],\n  "additionalProperties": false\n}'
  );
  let validationError = $state('');
  const mutation = createMutation(() => ({ mutationFn: runPlayground }));
  const modes = [
    { value: 'text', label: 'Text' },
    { value: 'tools', label: 'Tools' },
    { value: 'structured', label: 'Structured output' }
  ];

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    validationError = '';
    if (!model.trim()) {
      validationError = 'Enter an active route slug.';
      return;
    }
    if (!input.trim()) {
      validationError = 'Enter a prompt.';
      return;
    }
    let request: PlaygroundRequest;
    try {
      request = {
        model: model.trim(),
        input,
        surface,
        temperature: parseTemperature(temperature),
        max_output_tokens: parseMaxOutputTokens(maxOutputTokens),
        tools: mode === 'tools' ? parseTools(toolsJson) : undefined,
        response_format:
          mode === 'structured' ? parseResponseSchema(schemaJson) : undefined
      };
    } catch (error) {
      validationError = errorMessage(error, 'Check the request fields.');
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
    <div class="route-grid">
      <div class="form-field">
        <label for="playground-model">Route slug</label><input
          id="playground-model"
          bind:value={model}
          autocomplete="off"
          placeholder="support-chat"
          aria-describedby="model-help"
        /><small id="model-help"
          >The public model name clients use, not provider/model.</small
        >
      </div>
      <div class="form-field">
        <label for="playground-surface">Client surface</label><select
          id="playground-surface"
          bind:value={surface}
          ><option value="openai">OpenAI</option><option value="anthropic"
            >Anthropic</option
          ><option value="gemini">Gemini</option></select
        ><small>Capability filtering uses this originating protocol.</small>
      </div>
    </div>
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
    {#if validationError}<p class="field-error" role="alert">
        {validationError}
      </p>{/if}
    <button
      class="button button-primary"
      type="submit"
      disabled={mutation.isPending}
      >{mutation.isPending ? 'Running…' : 'Run test'}</button
    >
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
    {#if mutation.isPending}<div class="loading-state" role="status">
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
      {#if !mutation.data.refusal && !mutation.data.output_text && !mutation.data.tool_calls?.length && (mutation.data.structured_output === undefined || mutation.data.structured_output === null)}<div
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

<style>
  .privacy-note {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    margin-top: 1.25rem;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 35%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
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
    padding: 1.25rem;
  }
  .composer {
    display: grid;
    gap: 1rem;
  }
  .form-field input,
  .form-field textarea {
    width: 100%;
  }
  .form-field select {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.7rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .route-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.8rem;
  }
  .field-error {
    margin: 0;
    color: var(--danger);
    font-weight: 700;
  }
  .form-field small {
    display: block;
    margin-top: 0.25rem;
    color: var(--foreground-muted);
    font-size: 0.7rem;
  }
  .refusal {
    margin-top: 1rem;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--warning) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--warning-soft);
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
  h2,
  h3 {
    margin: 0;
  }
  h2 {
    font-size: 1.2rem;
  }
  h3 {
    margin-bottom: 0.4rem;
    font-size: 0.85rem;
  }
  .output {
    margin-top: 1rem;
  }
  pre {
    max-height: 28rem;
    margin: 0;
    overflow: auto;
    padding: 1rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    font-family: 'JetBrains Mono Variable', monospace;
    font-size: 0.78rem;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.75rem;
    margin: 1rem 0 0;
    padding-top: 1rem;
    border-top: 1px solid var(--border);
  }
  dt {
    color: var(--foreground-muted);
    font-size: 0.7rem;
    font-weight: 700;
  }
  dd {
    margin: 0.15rem 0 0;
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
      padding: 0.85rem;
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
