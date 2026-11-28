<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { readResult } from '#lib/editing/result.js';
  import CalculatorUsageFields from './CalculatorUsageFields.svelte';
  import { errorMessage } from '#lib/management/http.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { selectActiveRoutes } from '#lib/domain/routes/activeRoutes.js';
  import { routeQueries } from '#lib/domain/routes/queries.js';
  import { simulateRouting, decisionRows } from '#lib/features/routes/index.js';
  import type { ExplanationRow } from '#lib/features/routes/index.js';
  import { quoteUsage, type PricingQuote } from '#lib/features/usage/quote.js';
  import { providerQueries } from '#lib/domain/providers/queries.js';

  const services = useServiceCapabilities();

  let mode = $state<'route' | 'usage'>('route');

  // Route mode: a published simulation explains each target's price bounds.
  let route = $state('');
  let routeInput = $state('');
  let routeOutput = $state('');
  let routeResult = $state<ExplanationRow[] | null>(null);
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
    ...routeQueries.all(),
    enabled: services.gatewayAvailable,
    select: selectActiveRoutes
  }));
  const providers = createQuery(() => ({
    ...providerQueries.all(),
    enabled: services.gatewayAvailable
  }));
  const projects = createQuery(() => ({
    ...projectQueries.all(),
    enabled: services.gatewayAvailable
  }));

  $effect(() => {
    if (!routes.data?.some((item) => item.slug === route))
      route = routes.data?.[0]?.slug ?? '';
  });
  $effect(() => {
    if (!providerId && providers.data?.[0]) providerId = providers.data[0].id;
  });

  async function simulate() {
    routeError = '';
    routeResult = null;
    if (!route) {
      routeError = 'Choose a route.';
      return;
    }
    routeBusy = true;
    const result = await readResult(() =>
      simulateRouting({
        route,
        surface: 'openai',
        mode: 'streaming',
        estimatedInputTokens: routeInput.trim() ? Number(routeInput) : null,
        maxOutputTokensOverride: routeOutput.trim() ? Number(routeOutput) : null
      })
    );
    routeBusy = false;
    if (!result.ok) {
      routeError = errorMessage(result.error);
      return;
    }
    routeResult = decisionRows(
      result.value,
      new Map(
        (providers.data ?? []).map((provider) => [provider.id, provider.name])
      )
    );
  }

  async function runQuote() {
    quoteError = '';
    quoteResult = null;
    if (!providerId || !model.trim()) {
      quoteError = 'Choose a provider and name the model.';
      return;
    }
    quoteBusy = true;
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
    const cached = total('cached_input_tokens');
    const cachedAudio = total('cached_audio_input_tokens');
    const cachedImage = total('cached_image_input_tokens');
    const audio = total('audio_input_tokens');
    const image = total('image_input_tokens');
    const writes = total('cache_write_input_tokens');
    const inputs = cached + writes + audio - cachedAudio + image - cachedImage;
    const outputs = total('audio_output_tokens') + total('image_output_tokens');
    if (
      inputs > total('input_tokens') ||
      outputs > total('output_tokens') ||
      cachedAudio > audio ||
      cachedImage > image ||
      cachedAudio + cachedImage > cached ||
      total('cache_write_5m_input_tokens') +
        total('cache_write_1h_input_tokens') >
        writes
    ) {
      quoteError = 'Token shares must fit inside the input and output totals.';
      quoteBusy = false;
      return;
    }
    const calls: Record<string, number> = {};
    for (const row of toolCalls) {
      const name = row.name.trim();
      if (name) calls[name] = Math.max(1, Math.trunc(Number(row.count || '1')));
    }
    if (Object.keys(calls).length) usage.tool_calls = calls;
    if (mediaUnits.trim()) usage.media_units = mediaUnits.trim();
    if (batch) usage.batch = true;
    const result = await readResult(() =>
      quoteUsage({
        provider_id: providerId,
        model: model.trim(),
        operation,
        usage: usage as never,
        started_at: startedAt ? new Date(startedAt).toISOString() : undefined,
        successful,
        project_id: projectId || null
      })
    );
    quoteBusy = false;
    if (!result.ok) {
      quoteError = errorMessage(result.error);
      return;
    }
    quoteResult = result.value;
  }
</script>

{#if services.gatewayAvailable}
  <section class="card calculator" aria-labelledby="calculator-title">
    <div class="section-heading">
      <div>
        <h2 class="sr-only" id="calculator-title">Cost calculator</h2>
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
          onclick={() => (mode = 'route')}
        >
          Route
        </button><button
          role="tab"
          aria-selected={mode === 'usage'}
          class:active={mode === 'usage'}
          onclick={() => (mode = 'usage')}
        >
          Usage
        </button>
      </div>
    </div>

    {#if mode === 'route'}
      <div class="calculator-form">
        <label for="calc-route">Route</label><select
          id="calc-route"
          bind:value={route}
        >
          {#each routes.data ?? [] as option (option.id)}<option
              value={option.slug}
            >
              {option.slug}
            </option>{/each}
        </select>
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
        <button disabled={routeBusy} onclick={simulate}>
          {routeBusy ? 'Simulating…' : 'Simulate'}
        </button>
      </div>
      {#if routeError}<p class="field-error" role="alert">{routeError}</p>{/if}
      {#if routeResult}
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Target</th><th scope="col">Eligible</th><th
                  scope="col"
                >
                  Minimum
                </th><th scope="col">Maximum</th>
              </tr>
            </thead>
            <tbody>
              {#each routeResult as row (row.key)}<tr>
                  <td>
                    {row.title}<small>
                      {row.credentialSlotId ?? 'Connection authentication'}
                    </small>
                  </td><td>
                    {row.eligible
                      ? 'Yes'
                      : (row.reason?.replaceAll('_', ' ') ?? 'No')}
                  </td><td>
                    {row.estimatedCost
                      ? `${row.estimatedCost.minimum} ${row.estimatedCost.currency}`
                      : '—'}
                  </td><td>
                    {row.estimatedCost
                      ? `${row.estimatedCost.maximum} ${row.estimatedCost.currency}`
                      : '—'}
                  </td>
                </tr>{/each}
            </tbody>
          </table>
        </div>
      {/if}
    {:else}
      <CalculatorUsageFields
        bind:providerId
        bind:model
        bind:operation
        bind:inputTokens
        bind:outputTokens
        bind:cachedInputTokens
        bind:cacheWriteTokens
        bind:cacheWrite5mTokens
        bind:cacheWrite1hTokens
        bind:audioInputTokens
        bind:cachedAudioInputTokens
        bind:audioOutputTokens
        bind:imageInputTokens
        bind:cachedImageInputTokens
        bind:imageOutputTokens
        bind:mediaUnits
        bind:startedAt
        bind:projectId
        bind:batch
        bind:successful
        bind:toolCalls
        providers={providers.data ?? []}
        projects={projects.data ?? []}
        busy={quoteBusy}
        onSubmit={runQuote}
      />
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
  td small {
    display: block;
  }
</style>
