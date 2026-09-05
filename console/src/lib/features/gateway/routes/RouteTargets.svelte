<script lang="ts">
  import { resolve } from '$app/paths';
  import { eligibleTargetTuples, missingTargetOperations } from './routeEditor';
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
  let { editor }: { editor: RouteDraftEditorState } = $props();
</script>

<section class="card editor" aria-labelledby="targets-heading">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Attempt order</p>
      <h2 id="targets-heading">Eligible targets</h2>
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={editor.addTarget}
      disabled={!editor.canManage || !editor.modelOptions.length}
      >Add target</button
    >
  </div>
  {#if !editor.modelOptions.length}<div class="empty-state compact">
      <p>
        No enabled models are available. <a href={resolve('/models')}
          >Review model eligibility</a
        >.
      </p>
    </div>{/if}
  <ol class="targets">
    {#each editor.targets as target, index (index)}
      <li>
        <span class="target-number" aria-hidden="true">{index + 1}</span>
        <div class="target-fields">
          <div class="form-field model-select">
            <label for={`target-model-${index}`}>Provider model</label><select
              id={`target-model-${index}`}
              bind:value={target.providerModelId}
              onchange={editor.touch}
              disabled={!editor.canManage}
              >{#each editor.modelOptions as option (option.id)}<option
                  value={option.id}>{option.label}</option
                >{/each}</select
            >
          </div>
          <div class="form-field">
            <label for={`priority-${index}`}>Priority</label><input
              id={`priority-${index}`}
              type="number"
              min="1"
              max="100"
              bind:value={target.priority}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
          <div class="form-field">
            <label for={`weight-${index}`}>Weight</label><input
              id={`weight-${index}`}
              type="number"
              min="1"
              max="10000"
              bind:value={target.weight}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
          <div class="form-field">
            <label for={`timeout-${index}`}>Attempt timeout (ms)</label><input
              id={`timeout-${index}`}
              type="number"
              min="100"
              bind:value={target.timeoutMs}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
        </div>
        <button
          class="remove-target"
          type="button"
          aria-label={`Remove target ${index + 1}`}
          onclick={() => editor.removeTarget(index)}
          disabled={!editor.canManage}>×</button
        >
        <div
          class:warning={missingTargetOperations(
            target,
            editor.modelOptions,
            editor.operations
          ).length > 0}
          class="target-eligibility"
        >
          {#if eligibleTargetTuples(target, editor.modelOptions, editor.operations).length}
            <span
              ><strong>Certified tuples:</strong>
              {eligibleTargetTuples(
                target,
                editor.modelOptions,
                editor.operations
              ).join(', ')}</span
            >
          {:else}
            <span
              >No selected operation has a certified tuple on this target.</span
            >
          {/if}
          {#if missingTargetOperations(target, editor.modelOptions, editor.operations).length}<span
              ><strong>Missing:</strong>
              {missingTargetOperations(
                target,
                editor.modelOptions,
                editor.operations
              ).join(', ')}</span
            >{/if}
        </div>
      </li>
    {/each}
  </ol>
  {#if editor.routeEligibilityWarnings.length}<div
      class="eligibility-warning"
      role="status"
    >
      <strong>Route eligibility is incomplete.</strong><span
        >No selected target has a certified tuple for: {editor.routeEligibilityWarnings.join(
          ', '
        )}.</span
      >
    </div>{/if}
</section>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }

  .editor {
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
  }

  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .targets {
    display: grid;
    gap: 0.65rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
  }
  .targets li {
    display: grid;
    grid-template-columns: 2rem minmax(0, 1fr) 2.75rem;
    gap: 0.65rem;
    align-items: end;
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
  }
  .target-number {
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    margin-bottom: 0.35rem;
    border-radius: 50%;
    background: var(--accent-soft);
    color: var(--accent-strong);
    font: 750 0.72rem 'JetBrains Mono Variable';
  }
  .target-fields {
    display: grid;
    grid-column: 2;
    grid-template-columns: minmax(12rem, 2fr) repeat(3, minmax(7rem, 1fr));
    gap: 0.6rem;
  }
  .remove-target {
    grid-column: 3;
    grid-row: 1;
    width: 2.5rem;
    height: 2.5rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--danger);
    font-size: 1.3rem;
  }
  .target-eligibility {
    display: grid;
    grid-column: 2 / -1;
    gap: 0.2rem;
    color: var(--success);
    font-size: 0.72rem;
  }
  .target-eligibility.warning {
    color: var(--warning);
  }
  .eligibility-warning {
    display: grid;
    gap: 0.2rem;
    margin-top: 0.75rem;
    padding: 0.75rem;
    border: 1px solid color-mix(in srgb, var(--warning) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--warning-soft);
    color: var(--warning);
    font-size: 0.78rem;
  }

  .compact {
    min-height: 6rem;
  }
  .compact a {
    color: var(--accent-strong);
    font-weight: 700;
  }

  @media (max-width: 76rem) {
    .target-fields {
      grid-template-columns: repeat(3, 1fr);
    }
    .model-select {
      grid-column: 1 / -1;
    }
  }
  @media (max-width: 48rem) {
    .targets li {
      grid-template-columns: 1fr 2.75rem;
    }
    .target-number {
      display: none;
    }
    .target-fields {
      grid-column: 1;
      grid-template-columns: 1fr;
    }
    .remove-target {
      grid-column: 2;
    }
    .target-eligibility {
      grid-column: 1 / -1;
    }
    .model-select {
      grid-column: auto;
    }
  }
</style>
