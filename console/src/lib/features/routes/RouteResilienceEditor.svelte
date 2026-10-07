<script lang="ts">
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
  import {
    fallbackConditions,
    retryClasses,
    type FallbackCondition,
    type RetryClass
  } from './routeEditor';
  let { editor }: { editor: RouteDraftEditorState } = $props();
  const locked = $derived(!editor.canManage || Boolean(editor.busy));
  const selectorProblem = $derived(
    typeof editor.selectors === 'string' ? editor.selectors : ''
  );

  function addFallback() {
    editor.behavior.fallbacks.push({ route: '', on: ['exhausted'] });
    editor.touch();
  }
  function removeFallback(index: number) {
    editor.behavior.fallbacks.splice(index, 1);
    editor.touch();
  }
  function toggleCondition(index: number, condition: FallbackCondition) {
    const on = editor.behavior.fallbacks[index].on;
    const at = on.indexOf(condition);
    if (at < 0) on.push(condition);
    else on.splice(at, 1);
    editor.touch();
  }
  function toggleRetry(kind: RetryClass, enabled: boolean) {
    if (enabled)
      editor.behavior.retry[kind] = {
        max_retries: 1,
        base_backoff_ms: 200,
        max_backoff_ms: 2000,
        respect_retry_after: true
      };
    else delete editor.behavior.retry[kind];
    editor.touch();
  }
  function setAffinity(source: string) {
    editor.behavior.affinity =
      source === 'label'
        ? { source: 'label', label: '' }
        : source === 'cache_key'
          ? { source: 'cache_key' }
          : null;
    editor.touch();
  }
  function setCap(
    field: 'daily_cost_limit' | 'monthly_cost_limit',
    value: string
  ) {
    const budget = editor.behavior.budget ?? {
      daily_cost_limit: null,
      monthly_cost_limit: null
    };
    budget[field] = value.trim() || null;
    editor.behavior.budget =
      budget.daily_cost_limit || budget.monthly_cost_limit ? budget : null;
    editor.touch();
  }
</script>

<section class="card resilience" aria-labelledby="resilience-heading">
  <p class="eyebrow">Resilience</p>
  <h2 id="resilience-heading">Fallbacks, selectors and retries</h2>

  <fieldset>
    <legend>Fallback routes</legend>
    <p class="muted">
      When this route cannot serve a request for a named reason, OLP tries these
      routes in order, within this route's deadline and attempt budget. A
      committed stream never falls back.
    </p>
    {#each editor.behavior.fallbacks as fallback, index (index)}
      <div class="fallback">
        <div class="form-field">
          <label for={`fallback-route-${index}`}>Route slug</label><input
            id={`fallback-route-${index}`}
            bind:value={fallback.route}
            oninput={editor.touch}
            disabled={locked}
          />
        </div>
        <div
          class="conditions"
          role="group"
          aria-label={`Conditions for fallback ${index + 1}`}
        >
          {#each fallbackConditions as [condition, label] (condition)}
            <label class="check"
              ><input
                type="checkbox"
                checked={fallback.on.includes(condition)}
                onchange={() => toggleCondition(index, condition)}
                disabled={locked}
              />{label}</label
            >
          {/each}
        </div>
        <button
          type="button"
          class="secondary"
          aria-label={`Remove fallback ${index + 1}`}
          onclick={() => removeFallback(index)}
          disabled={locked}>Remove</button
        >
      </div>
    {/each}
    <button
      type="button"
      class="secondary"
      onclick={addFallback}
      disabled={locked || editor.behavior.fallbacks.length >= 4}
      >Add fallback</button
    >
  </fieldset>

  <fieldset>
    <legend>Retries on the same target</legend>
    <p class="muted">
      Retries wait with full jitter, count against the attempt budget and the
      deadline, and never follow a committed response or an ambiguous creation.
    </p>
    {#each retryClasses as [kind, label] (kind)}
      {@const rule = editor.behavior.retry[kind]}
      <div class="retry">
        <label class="check"
          ><input
            type="checkbox"
            checked={Boolean(rule)}
            onchange={(event) => toggleRetry(kind, event.currentTarget.checked)}
            disabled={locked}
          />{label}</label
        >
        {#if rule}
          <div class="form-field">
            <label for={`retry-${kind}-count`}>Retries</label><input
              id={`retry-${kind}-count`}
              type="number"
              min="0"
              max="10"
              bind:value={rule.max_retries}
              oninput={editor.touch}
              disabled={locked}
            />
          </div>
          <div class="form-field">
            <label for={`retry-${kind}-base`}>Base backoff (ms)</label><input
              id={`retry-${kind}-base`}
              type="number"
              min="0"
              max="60000"
              bind:value={rule.base_backoff_ms}
              oninput={editor.touch}
              disabled={locked}
            />
          </div>
          <div class="form-field">
            <label for={`retry-${kind}-max`}>Maximum backoff (ms)</label><input
              id={`retry-${kind}-max`}
              type="number"
              min="0"
              max="60000"
              bind:value={rule.max_backoff_ms}
              oninput={editor.touch}
              disabled={locked}
            />
          </div>
          <label class="check"
            ><input
              type="checkbox"
              bind:checked={rule.respect_retry_after}
              onchange={editor.touch}
              disabled={locked}
            />Honor Retry-After</label
          >
        {/if}
      </div>
    {/each}
  </fieldset>

  <div class="form-grid">
    <div class="form-field">
      <label for="affinity-source">Session affinity</label><select
        id="affinity-source"
        value={editor.behavior.affinity?.source ?? ''}
        onchange={(event) => setAffinity(event.currentTarget.value)}
        disabled={locked}
        aria-describedby="affinity-help"
      >
        <option value="">None</option>
        <option value="cache_key">Prompt cache key</option>
        <option value="label">Attribution label</option>
      </select>
      <small id="affinity-help"
        >Keeps one session on the same target and slot while it stays eligible,
        raising provider prompt-cache hits.</small
      >
    </div>
    {#if editor.behavior.affinity?.source === 'label'}
      <div class="form-field">
        <label for="affinity-label">Attribution label</label><input
          id="affinity-label"
          bind:value={editor.behavior.affinity.label}
          oninput={editor.touch}
          disabled={locked}
        />
      </div>
    {/if}
    <div class="form-field">
      <label for="route-daily-cap">Daily spend cap</label><input
        id="route-daily-cap"
        inputmode="decimal"
        placeholder="No cap"
        value={editor.behavior.budget?.daily_cost_limit ?? ''}
        oninput={(event) =>
          setCap('daily_cost_limit', event.currentTarget.value)}
        disabled={locked}
      />
    </div>
    <div class="form-field">
      <label for="route-monthly-cap">Monthly spend cap</label><input
        id="route-monthly-cap"
        inputmode="decimal"
        placeholder="No cap"
        value={editor.behavior.budget?.monthly_cost_limit ?? ''}
        oninput={(event) =>
          setCap('monthly_cost_limit', event.currentTarget.value)}
        disabled={locked}
      />
    </div>
  </div>

  <div class="form-field selectors">
    <label for="route-selectors">Request selectors (JSON)</label>
    <textarea
      id="route-selectors"
      rows="6"
      spellcheck="false"
      bind:value={editor.selectorsText}
      oninput={editor.touch}
      disabled={locked}
      aria-describedby="selectors-help"
      aria-invalid={Boolean(selectorProblem)}></textarea>
    <small id="selectors-help"
      >The first selector whose predicate matches narrows the route to targets
      carrying its tags, or delegates to another route. For example
      <code
        >[{'{'}"id": "short", "when": {'{'}"max_input_tokens": 2000}, "tags":
        ["small"]}]</code
      >. Simulation explains which selector matched.</small
    >
    {#if selectorProblem}<p class="inline-problem" role="alert">
        {selectorProblem}
      </p>{/if}
  </div>
</section>

<style>
  .resilience {
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
    display: grid;
    gap: 1.1rem;
  }
  h2 {
    margin: 0.3rem 0 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
    display: grid;
    gap: 0.6rem;
  }
  legend {
    font-weight: 500;
    margin-bottom: 0.3rem;
  }
  .fallback,
  .retry {
    display: grid;
    grid-template-columns: minmax(10rem, 1fr) minmax(0, 2fr) auto;
    gap: 0.6rem;
    align-items: end;
  }
  .retry {
    grid-template-columns: minmax(12rem, 1fr) repeat(3, minmax(7rem, 1fr)) auto;
  }
  .conditions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem 0.9rem;
  }
  .check {
    display: inline-flex;
    gap: 0.4rem;
    align-items: center;
  }
  textarea {
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  small,
  .muted {
    color: var(--foreground-subtle);
    font-size: var(--text-caption);
  }
  code {
    font-family: var(--font-mono);
  }
  @media (max-width: 64rem) {
    .fallback,
    .retry {
      grid-template-columns: 1fr;
    }
  }
</style>
