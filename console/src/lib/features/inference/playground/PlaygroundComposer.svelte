<script lang="ts">
  import { RoutingPreferencesForm } from '#lib/features/routes/index.js';
  import SegmentedRadioGroup from '#lib/shell/SegmentedRadioGroup.svelte';
  import PlaygroundDryRun from './PlaygroundDryRun.svelte';
  import { playgroundTemplates } from './templates';
  import { errorMessage } from '#lib/management/http.js';
  import type { PlaygroundState } from './playgroundState.svelte';

  let { playground }: { playground: PlaygroundState } = $props();

  const surfaceLocked = $derived(
    playground.composer === 'advanced' &&
      playground.operation !== 'generation' &&
      playground.operation !== 'token_count'
  );
  const inspectionSurface = $derived(playground.plan.inspection.surface);
  const clientSurface = $derived(
    playground.activeOperation === 'decision' &&
      (inspectionSurface === 'openai' || inspectionSurface === 'gemini')
      ? inspectionSurface
      : playground.surface
  );
  const operations = [
    { value: 'generation', label: 'Generation' },
    { value: 'decision', label: 'Decisions' },
    { value: 'ocr', label: 'OCR' },
    { value: 'search', label: 'Web search' },
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
        {#if !playground.operationKnown}<small class="field-error" role="alert"
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
          The local trace viewer below does not use a route or request template
          and starts no session.
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
          >This route enforces an output content policy — streaming requests are
          rejected. Playground runs are unary and still permitted.</small
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
        bind:value={
          () => clientSurface,
          (surface: PlaygroundState['surface']) => {
            playground.surface = surface;
          }
        }
        disabled={surfaceLocked}
        ><option value="openai">OpenAI</option><option value="anthropic"
          >Anthropic</option
        ><option value="gemini">Gemini</option></select
      >
      {#if !surfaceLocked}
        <small>Capability filtering uses this originating protocol.</small>
      {/if}
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
        /><small id="max-output-help">1 through 1048576.</small>
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
        >Raw public request body. The route slug above replaces its model field
        — provider addresses cannot be set here.</small
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
  <PlaygroundDryRun {playground} />

  <div class="run-actions">
    <button
      class="button button-primary"
      type="submit"
      disabled={playground.mutation.isPending ||
        playground.streaming ||
        Boolean(playground.plan.submissionError)}
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

<style>
  .composer {
    display: grid;
    gap: 1rem;
    padding: 1.5rem;
  }
  .route-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.8rem;
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
  @media (max-width: 38rem) {
    .composer {
      padding: 1rem;
    }
    .route-grid {
      display: grid;
      grid-template-columns: 1fr;
    }
  }
</style>
