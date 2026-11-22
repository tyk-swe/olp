<script lang="ts">
  import { errorMessage } from '#lib/management/http.js';
  import { readResult } from '#lib/editing/result.js';
  import {
    evaluatePolicy as evaluatePolicyAPI,
    type Entry,
    type Policy,
    type Result
  } from './api';

  let {
    capture,
    policies,
    phases,
    busy,
    canEvaluate,
    evaluating = $bindable(),
    error = $bindable(),
    evaluatePolicy = $bindable(),
    phase = $bindable(),
    text = $bindable(),
    result = $bindable()
  }: {
    capture: () => () => boolean;
    policies: Policy[];
    phases: readonly Entry['phase'][];
    busy: boolean;
    canEvaluate: boolean;
    evaluating: boolean;
    error: string;
    evaluatePolicy: string;
    phase: Entry['phase'];
    text: string;
    result: Result | null;
  } = $props();

  async function evaluate(event: SubmitEvent) {
    event.preventDefault();
    const current = capture();
    evaluating = true;
    error = '';
    result = null;
    const evaluated = await readResult(() =>
      evaluatePolicyAPI({
        policy_revision_id: evaluatePolicy,
        phase,
        segments: [{ id: 'text', role: 'user', text }]
      })
    );
    if (!current()) return;
    evaluating = false;
    if (evaluated.ok) result = evaluated.value;
    else error = errorMessage(evaluated.error);
  }
</script>

<section class="card stack-card">
  <h2>Try a policy</h2>
  <p>
    Test a policy without saving the content you enter. External guardrails send
    it to their configured endpoints.
  </p>
  <form onsubmit={evaluate}>
    <label class="form-field"
      >Policy revision<select required bind:value={evaluatePolicy}
        ><option value="">Choose a policy</option
        >{#each policies as value (value.policy_id)}<option
            value={value.revision_id}
            >{value.name} · Revision {value.revision}</option
          >{/each}</select
      ></label
    >
    <label class="form-field"
      >Phase<select bind:value={phase}
        >{#each phases as value (value)}<option {value}>{value}</option
          >{/each}</select
      ></label
    >
    <label class="form-field"
      >Text<textarea rows="8" required bind:value={text}></textarea></label
    ><button class="button button-secondary" disabled={busy || !canEvaluate}
      >Evaluate</button
    ><button
      class="button button-secondary"
      type="button"
      onclick={() => {
        text = '';
        result = null;
      }}>Clear content</button
    >
  </form>
  {#if result}<div role="status">
      <h3>{result.blocked ? 'Blocked' : 'Allowed'}</h3>
      <ul>
        {#each result.decisions as decision (`${decision.guardrail_id}-${decision.phase}-${decision.outcome}`)}<li
          >
            {decision.guardrail_id} · {decision.phase} · {decision.outcome} · {(
              decision.categories ?? []
            ).join(', ')}
          </li>{/each}
      </ul>
      <pre class="code-block">{result.segments
          .map((segment) => segment.text)
          .join('\n')}</pre>
    </div>{/if}
</section>

<style>
  form {
    display: grid;
    gap: 1rem;
    max-width: 52rem;
  }
  form > .button {
    justify-self: start;
  }
</style>
