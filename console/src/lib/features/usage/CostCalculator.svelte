<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import { listProviders } from '$lib/features/providers/api/providers';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { listProjects } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { listRoutes, simulateRouting } from '$lib/features/routes/api';
  import { routeKeys } from '$lib/features/routes/routeKeys';
  import { operationKinds } from '$lib/features/usage/history/api';
  import { quoteUsage, type PricingQuote } from '$lib/features/usage/api/quote';

  const services = useServiceCapabilities();

  let mode = $state<'route' | 'usage'>('route');

  // Route mode: a published simulation explains each target's price bounds.
  let route = $state('');
  let routeInput = $state('');
  let routeOutput = $state('');
  let routeResult = $state<
    | {
        target: string;
        eligible: boolean;
        reason: string | null;
        minimum: string;
        maximum: string;
        currency: string;
      }[]
    | null
  >(null);
  let routeError = $state('');
  let routeBusy = $state(false);

  // Usage mode: price one attempt's usage exactly as accounting records it.
  let providerId = $state('');
  let model = $state('');
  let operation = $state('generation');
  let inputTokens = $state('');
  let outputTokens = $state('');
  let cachedInputTokens = $state('');
  let cacheWriteTokens = $state('');
  let cacheWrite5mTokens = $state('');
  let cacheWrite1hTokens = $state('');
  let audioInputTokens = $state('');
  let cachedAudioInputTokens = $state('');
  let audioOutputTokens = $state('');
  let imageInputTokens = $state('');
  let cachedImageInputTokens = $state('');
  let imageOutputTokens = $state('');
  let toolCalls = $state<{ name: string; count: string }[]>([]);
  let mediaUnits = $state('');
  let batch = $state(false);
  let successful = $state(true);
  let startedAt = $state('');
  let projectId = $state('');
  let quoteResult = $state<PricingQuote | null>(null);
  let quoteError = $state('');
  let quoteBusy = $state(false);

  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  const providers = createQuery(() => ({
    queryKey: providerKeys.all(),
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listProviders(signal)
  }));
  const projects = createQuery(() => ({
    queryKey: projectKeys.all,
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listProjects(signal)
  }));

  async function simulate() {
    routeError = '';
    routeResult = null;
    if (!route) {
      routeError = 'Choose a route.';
      return;
    }
    routeBusy = true;
    try {
      const decisions = await simulateRouting({
        route,
        surface: 'openai',
        mode: 'streaming',
        estimatedInputTokens: routeInput.trim() ? Number(routeInput) : null,
        maxOutputTokensOverride: routeOutput.trim() ? Number(routeOutput) : null
      });
      routeResult = decisions.map((decision) => ({
        target: decision.upstream_model,
        eligible: decision.eligible,
        reason: decision.reason ?? null,
        minimum: decision.estimated_cost?.minimum ?? '',
        maximum: decision.estimated_cost?.maximum ?? '',
        currency: decision.estimated_cost?.currency ?? ''
      }));
    } catch (failure) {
      routeError = errorMessage(failure);
    } finally {
      routeBusy = false;
    }
  }

  async function runQuote() {
    quoteError = '';
    quoteResult = null;
    if (!providerId || !model.trim()) {
      quoteError = 'Choose a provider and name the model.';
      return;
    }
    quoteBusy = true;
    try {
      const usage: Record<string, unknown> = {};
      const tokens: [string, string][] = [
        ['input_tokens', inputTokens],
        ['output_tokens', outputTokens],
        ['cached_input_tokens', cachedInputTokens],
        ['cache_write_input_tokens', cacheWriteTokens],
        ['cache_write_5m_input_tokens', cacheWrite5mTokens],
        ['cache_write_1h_input_tokens', cacheWrite1hTokens],
        ['audio_input_tokens', audioInputTokens],
        ['cached_audio_input_tokens', cachedAudioInputTokens],
        ['audio_output_tokens', audioOutputTokens],
        ['image_input_tokens', imageInputTokens],
        ['cached_image_input_tokens', cachedImageInputTokens],
        ['image_output_tokens', imageOutputTokens]
      ];
      for (const [key, raw] of tokens) {
        if (raw.trim()) usage[key] = Math.max(0, Math.trunc(Number(raw)));
      }
      // Shares stay inside the totals they are part of, as events require.
      const total = (key: string) => Number(usage[key] ?? 0);
      const inputs =
        total('cached_input_tokens') +
        total('cache_write_input_tokens') +
        total('cache_write_5m_input_tokens') +
        total('cache_write_1h_input_tokens') +
        total('audio_input_tokens') +
        total('cached_audio_input_tokens') +
        total('image_input_tokens') +
        total('cached_image_input_tokens');
      const outputs =
        total('audio_output_tokens') + total('image_output_tokens');
      if (inputs > total('input_tokens') || outputs > total('output_tokens')) {
        quoteError =
          'Token shares must fit inside the input and output totals.';
        quoteBusy = false;
        return;
      }
      const calls: Record<string, number> = {};
      for (const row of toolCalls) {
        const name = row.name.trim();
        if (name)
          calls[name] = Math.max(1, Math.trunc(Number(row.count || '1')));
      }
      if (Object.keys(calls).length) usage.tool_calls = calls;
      if (mediaUnits.trim()) usage.media_units = mediaUnits.trim();
      if (batch) usage.batch = true;
      quoteResult = await quoteUsage({
        provider_id: providerId,
        model: model.trim(),
        operation,
        usage: usage as never,
        started_at: startedAt ? new Date(startedAt).toISOString() : undefined,
        successful,
        project_id: projectId || null
      });
    } catch (failure) {
      quoteError = errorMessage(failure);
    } finally {
      quoteBusy = false;
    }
  }
</script>

{#if services.gatewayAvailable}
  <section class="card calculator" aria-labelledby="calculator-title">
    <div class="section-heading">
      <div>
        <h2 id="calculator-title">Cost calculator</h2>
        <p>
          Bound a route's attempt costs or price usage exactly as accounting
          records it.
        </p>
      </div>
      <div role="tablist" aria-label="Calculator mode">
        <button
          role="tab"
          aria-selected={mode === 'route'}
          class:active={mode === 'route'}
          onclick={() => (mode = 'route')}>Route</button
        ><button
          role="tab"
          aria-selected={mode === 'usage'}
          class:active={mode === 'usage'}
          onclick={() => (mode = 'usage')}>Usage</button
        >
      </div>
    </div>

    {#if mode === 'route'}
      <div class="calculator-form">
        <label for="calc-route">Route</label><select
          id="calc-route"
          bind:value={route}
          >{#each routes.data ?? [] as option (option.id)}<option
              value={option.slug}>{option.slug}</option
            >{/each}</select
        >
        <label for="calc-route-input">Input tokens</label><input
          id="calc-route-input"
          bind:value={routeInput}
          inputmode="numeric"
          placeholder="Counted estimate"
        />
        <label for="calc-route-output">Output token limit</label><input
          id="calc-route-output"
          bind:value={routeOutput}
          inputmode="numeric"
          placeholder="Default reply allowance"
        />
        <button disabled={routeBusy} onclick={simulate}
          >{routeBusy ? 'Simulating…' : 'Simulate'}</button
        >
      </div>
      {#if routeError}<p class="field-error" role="alert">{routeError}</p>{/if}
      {#if routeResult}
        <div class="table-wrap">
          <table>
            <thead
              ><tr
                ><th scope="col">Target</th><th scope="col">Eligible</th><th
                  scope="col">Minimum</th
                ><th scope="col">Maximum</th></tr
              ></thead
            >
            <tbody
              >{#each routeResult as row (row.target)}<tr
                  ><td>{row.target}</td><td
                    >{row.eligible
                      ? 'Yes'
                      : (row.reason?.replaceAll('_', ' ') ?? 'No')}</td
                  ><td
                    >{row.minimum ? `${row.minimum} ${row.currency}` : '—'}</td
                  ><td
                    >{row.maximum ? `${row.maximum} ${row.currency}` : '—'}</td
                  ></tr
                >{/each}</tbody
            >
          </table>
        </div>
      {/if}
    {:else}
      <div class="calculator-form">
        <label for="calc-provider">Provider</label><select
          id="calc-provider"
          bind:value={providerId}
          >{#each providers.data ?? [] as provider (provider.id)}<option
              value={provider.id}>{provider.name}</option
            >{/each}</select
        >
        <label for="calc-model">Model</label><input
          id="calc-model"
          bind:value={model}
        />
        <label for="calc-operation">Operation</label><select
          id="calc-operation"
          bind:value={operation}
          >{#each operationKinds as option (option)}<option value={option}
              >{option.replaceAll('_', ' ')}</option
            >{/each}</select
        >
        <label for="calc-input">Input tokens</label><input
          id="calc-input"
          bind:value={inputTokens}
          inputmode="numeric"
        />
        <label for="calc-output">Output tokens</label><input
          id="calc-output"
          bind:value={outputTokens}
          inputmode="numeric"
        />
        <label for="calc-cached">Cached input tokens</label><input
          id="calc-cached"
          bind:value={cachedInputTokens}
          inputmode="numeric"
        />
        <details class="more-usage">
          <summary>More usage</summary>
          <label for="calc-cache-write">Cache write tokens</label><input
            id="calc-cache-write"
            bind:value={cacheWriteTokens}
            inputmode="numeric"
          />
          <label for="calc-cache-write-5m">5-minute cache writes</label><input
            id="calc-cache-write-5m"
            bind:value={cacheWrite5mTokens}
            inputmode="numeric"
          />
          <label for="calc-cache-write-1h">1-hour cache writes</label><input
            id="calc-cache-write-1h"
            bind:value={cacheWrite1hTokens}
            inputmode="numeric"
          />
          <label for="calc-audio-input">Audio input tokens</label><input
            id="calc-audio-input"
            bind:value={audioInputTokens}
            inputmode="numeric"
          />
          <label for="calc-cached-audio-input">Cached audio input tokens</label
          ><input
            id="calc-cached-audio-input"
            bind:value={cachedAudioInputTokens}
            inputmode="numeric"
          />
          <label for="calc-audio-output">Audio output tokens</label><input
            id="calc-audio-output"
            bind:value={audioOutputTokens}
            inputmode="numeric"
          />
          <label for="calc-image-input">Image input tokens</label><input
            id="calc-image-input"
            bind:value={imageInputTokens}
            inputmode="numeric"
          />
          <label for="calc-cached-image-input">Cached image input tokens</label
          ><input
            id="calc-cached-image-input"
            bind:value={cachedImageInputTokens}
            inputmode="numeric"
          />
          <label for="calc-image-output">Image output tokens</label><input
            id="calc-image-output"
            bind:value={imageOutputTokens}
            inputmode="numeric"
          />
          {#each toolCalls as row, index (index)}
            <div class="tool-call-row">
              <label class="sr-only" for="calc-tool-name-{index}"
                >Tool name</label
              ><input
                id="calc-tool-name-{index}"
                bind:value={row.name}
                placeholder="tool name"
              />
              <label class="sr-only" for="calc-tool-count-{index}">Calls</label
              ><input
                id="calc-tool-count-{index}"
                bind:value={row.count}
                inputmode="numeric"
                placeholder="count"
              />
              <button
                type="button"
                class="link"
                aria-label="Remove tool call"
                onclick={() => toolCalls.splice(index, 1)}>Remove</button
              >
            </div>
          {/each}
          <button
            type="button"
            class="link"
            onclick={() => toolCalls.push({ name: '', count: '1' })}
            >Add tool call</button
          >
        </details>
        <label for="calc-media">Media units</label><input
          id="calc-media"
          bind:value={mediaUnits}
          inputmode="decimal"
        />
        <label for="calc-started">Started at</label><input
          id="calc-started"
          type="datetime-local"
          bind:value={startedAt}
        />
        <label for="calc-project">Consumer project</label><select
          id="calc-project"
          bind:value={projectId}
          ><option value="">Installation card</option
          >{#each projects.data ?? [] as project (project.id)}<option
              value={project.id}>{project.name}</option
            >{/each}</select
        >
        <label class="check"
          ><input type="checkbox" bind:checked={batch} />Batch discount applies</label
        >
        <label class="check"
          ><input type="checkbox" bind:checked={successful} />Successful attempt</label
        >
        <button disabled={quoteBusy} onclick={runQuote}
          >{quoteBusy ? 'Pricing…' : 'Quote'}</button
        >
      </div>
      {#if quoteError}<p class="field-error" role="alert">{quoteError}</p>{/if}
      {#if quoteResult}
        <dl class="quote-result">
          <dt>Cost</dt>
          <dd>{quoteResult.cost ?? 'unpriced'} {quoteResult.currency ?? ''}</dd>
          <dt>List cost</dt>
          <dd>{quoteResult.list_cost ?? '—'}</dd>
          <dt>Charge</dt>
          <dd>{quoteResult.charge ?? '—'}</dd>
          {#if quoteResult.commitment}<dt>Commitment</dt>
            <dd>
              {quoteResult.commitment.amount} per {quoteResult.commitment
                .period}
            </dd>{/if}
          {#if quoteResult.components.length}<dt>Components</dt>
            <dd>{quoteResult.components.join(' · ')}</dd>{/if}
          {#if !quoteResult.complete}<dt>Completeness</dt>
            <dd>Some dimensions have no rate.</dd>{/if}
        </dl>
      {/if}
    {/if}
  </section>
{/if}

<style>
  .calculator {
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .calculator-form {
    display: grid;
    grid-template-columns: 12rem minmax(0, 20rem);
    gap: 0.6rem 1rem;
    align-items: center;
    margin: 1rem 0;
  }
  .check {
    grid-column: 2;
  }
  button {
    width: fit-content;
  }
  [role='tablist'] button {
    border: 1px solid var(--border-hairline);
    padding: 0.3rem 0.9rem;
  }
  [role='tablist'] .active {
    background: var(--accent-soft);
  }
  .quote-result {
    display: grid;
    grid-template-columns: 10rem minmax(0, 1fr);
    gap: 0.35rem 0.75rem;
  }
  .quote-result dt {
    color: var(--foreground-muted);
  }
  .table-wrap {
    overflow-x: auto;
  }
  .more-usage {
    grid-column: 1 / -1;
    display: grid;
    grid-template-columns: 12rem minmax(0, 20rem);
    gap: 0.6rem 1rem;
    align-items: center;
  }
  .more-usage summary {
    grid-column: 1 / -1;
    cursor: pointer;
    color: var(--foreground-muted);
  }
  .tool-call-row {
    display: contents;
  }
  .tool-call-row > * {
    grid-column: auto;
  }
  .link {
    width: fit-content;
    padding: 0;
    background: none;
    border: none;
    color: var(--accent);
    cursor: pointer;
    text-decoration: underline;
  }
</style>
