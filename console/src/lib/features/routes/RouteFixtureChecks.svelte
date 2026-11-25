<script lang="ts">
  import { useAction } from '#lib/editing/action.svelte.js';
  import { formatDate, formatInteger } from '#lib/format.js';
  import {
    checkRouteFixtures,
    type RouteFixturePlan,
    type RouteFixtureReport
  } from './api.js';
  import { inputBreakdownRows } from './routingExplanation.js';

  let {
    draftId,
    draftEtag,
    disabled
  }: {
    draftId: string;
    draftEtag: string;
    disabled: boolean;
  } = $props();
  const action = useAction();
  let file = $state<File | null>(null);
  let report = $state<RouteFixtureReport | null>(null);
  const describe = (code: string) =>
    code === 'classifier_not_simulated'
      ? 'Classifier verdict omitted'
      : code.replaceAll('_', ' ');

  const numberOrUnknown = (count: number | null) =>
    count === null ? 'Unknown' : formatInteger(count);

  function selectFile(selected: File | null) {
    report = null;
    if (selected && selected.size > 256 * 1024) {
      file = null;
      action.error = 'Use a JSON file of at most 256 KiB.';
      return;
    }
    file = selected;
    action.error = '';
  }

  function compare() {
    if (!file || disabled || action.busy) return;
    const selected = file;
    report = null;
    void action.run(
      () => checkRouteFixtures(draftId, selected),
      (result) => {
        if (result.draft_etag !== draftEtag) {
          action.error =
            'The saved draft changed. Reload it before comparing fixtures.';
          return;
        }
        report = result;
      }
    );
  }
</script>

<section
  id="route-fixtures"
  class="card fixture-checks"
  aria-labelledby="fixture-heading"
>
  <h2 id="fixture-heading">Check saved request fixtures</h2>
  <p id="fixture-file-help">
    Compare the saved draft with the active published revision. Use a local JSON
    file with up to 16 synthetic fixtures (256 KiB total). Do not include real
    prompts, secrets or personal data. The check sends no upstream requests and
    does not store the file or request bodies.
  </p>
  <div class="controls">
    <div class="form-field">
      <label for="route-fixture-file">Synthetic fixture file</label>
      <input
        id="route-fixture-file"
        type="file"
        accept=".json,application/json"
        aria-describedby="fixture-file-help"
        disabled={action.busy}
        onchange={(event) => selectFile(event.currentTarget.files?.[0] ?? null)}
      />
    </div>
    <button
      class="button button-secondary"
      type="button"
      disabled={!file || disabled || action.busy}
      onclick={compare}
    >
      {action.busy ? 'Comparing…' : 'Compare fixtures'}
    </button>
  </div>
  {#if disabled && !action.busy}<p role="status">
      Save draft and routing policy changes before comparing.
    </p>{/if}
  {#if action.error}<p role="alert">{action.error}</p>{/if}
  {#if report && !disabled}
    <div role="status">
      <strong>
        {report.passed ? 'No failed assertions' : 'Fixture assertions failed'}
      </strong>
      <p>
        Compared with published revision {report.published_revision} at {formatDate(
          report.planning_time
        )}. Results can change when planning data changes. Live capacity, spend
        and circuit state are not simulated. Configured spend caps without a
        reading are treated as unknown.
      </p>
    </div>
    {#each report.results as result (result.name)}
      <details
        class="fixture"
        open={result.changes.length > 0 || !result.passed}
      >
        <summary>
          <strong>{result.name}</strong>
          <span class="badge">
            {result.expect_eligible === null
              ? 'No assertion'
              : result.passed
                ? 'Passed'
                : 'Failed'}
          </span>
          <span>
            {result.changes.length
              ? `Changed: ${result.changes.join(', ')}`
              : 'Unchanged'}
          </span>
        </summary>
        {#if result.expect_eligible !== null}
          <p>
            {result.expect_eligible
              ? 'Requires a serving attempt in the draft.'
              : 'Requires no serving attempt in the draft.'}
          </p>
        {/if}
        <div class="plans">
          {@render planView('Published', result.published)}
          {@render planView('Draft', result.draft)}
        </div>
      </details>
    {/each}
  {/if}
</section>

{#snippet planView(label: string, plan: RouteFixturePlan)}
  <section class="plan" aria-label={label}>
    <h3>{label}</h3>
    <p>
      {#if plan.winner}
        First attempt: {plan.winner.provider_name || plan.winner.provider_id} / {plan
          .winner.model} ({plan.winner.route})
      {:else}
        No serving attempt{plan.reason ? ` (${describe(plan.reason)})` : ''}
      {/if}
    </p>
    {#if plan.selectors.length}
      <h4>Selectors</h4>
      <ul>
        {#each plan.selectors as selector (`${selector.route}:${selector.id}`)}
          <li>
            {selector.id} ({selector.route}): {describe(selector.outcome)}
          </li>
        {/each}
      </ul>
    {/if}
    {#if plan.fallbacks.length}
      <h4>Fallbacks</h4>
      <ul>
        {#each plan.fallbacks as fallback, index (`${index}:${fallback.route}`)}
          <li>{fallback.route}: {describe(fallback.outcome)}</li>
        {/each}
      </ul>
    {/if}
    {#if plan.targets.length}
      <h4>Target decisions</h4>
      {#each plan.targets as target, index (`${index}:${target.provider_id}:${target.model}`)}
        <details>
          <summary>
            {target.provider_name || target.provider_id} / {target.model} ({target.route}):
            {target.shadow
              ? 'Shadow'
              : target.eligible
                ? 'Eligible'
                : 'Excluded'}{target.reason
              ? ` (${describe(target.reason)})`
              : ''}
          </summary>
          {#if target.context_window}
            {@const window = target.context_window}
            <p>
              Context limit {numberOrUnknown(target.context_length)}. Estimated
              input {numberOrUnknown(
                target.estimated_input_tokens
              )}{target.estimate_provenance
                ? ` (${describe(target.estimate_provenance)})`
                : ''}. Output reservation {target.requested_output_tokens ===
              null
                ? 'none'
                : formatInteger(target.requested_output_tokens)}. Headroom {numberOrUnknown(
                window.remaining_tokens
              )}.
              {window.retained_context_unknown
                ? 'Provider-held context is unmeasured.'
                : ''}
            </p>
            {#if window.input_breakdown}
              <ul aria-label="Estimated input tokens by category">
                {#each inputBreakdownRows(window.input_breakdown) as part (part.label)}
                  <li>{part.label}: {formatInteger(part.tokens)}</li>
                {/each}
              </ul>
            {/if}
          {:else}
            <p>No context window estimate for this request.</p>
          {/if}
        </details>
      {/each}
    {/if}
  </section>
{/snippet}

<style>
  .fixture-checks {
    margin-top: 1rem;
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
    scroll-margin-top: 5rem;
  }

  .controls {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 0.75rem;
    margin: 1rem 0;
  }

  .fixture {
    border-top: 1px solid var(--border-hairline);
    padding: 0.75rem 0;
  }

  summary {
    cursor: pointer;
    overflow-wrap: anywhere;
  }

  .plans {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.75rem;
    margin-top: 0.75rem;
  }

  .plan {
    padding: 0.75rem;
    background: var(--surface-raised);
    border-radius: var(--radius-control);
    overflow-wrap: anywhere;
  }

  h4 {
    margin-bottom: 0.25rem;
  }

  @media (max-width: 48rem) {
    .plans {
      grid-template-columns: 1fr;
    }
  }
</style>
