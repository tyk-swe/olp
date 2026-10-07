<script lang="ts">
  /** Comma-separated tags, as the operator types them. */
  function splitTags(text: string): string[] {
    return text
      .split(',')
      .map((tag) => tag.trim())
      .filter(Boolean);
  }
  import { resolve } from '$app/paths';
  import {
    eligibleTargetTuples,
    missingTargetOperations,
    storedTargetLabel,
    targetUnavailable
  } from '$lib/features/routes/routeEditor';
  import type { RouteDraftEditorState } from '$lib/features/routes/routeDraftEditor.svelte';
  import { lifecycleNotice } from '$lib/features/providers/models/lifecycle';
  import type { EditableTarget } from '$lib/features/routes/routeEditor';
  let { editor }: { editor: RouteDraftEditorState } = $props();

  function targetNotice(target: EditableTarget) {
    return lifecycleNotice(
      editor.modelOptions.find((option) => option.id === target.providerModelId)
        ?.lifecycle
    );
  }
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
      {@const notice = targetNotice(target)}
      <li>
        <span class="target-number" aria-hidden="true">{index + 1}</span>
        <div class="target-fields">
          <div class="form-field model-select">
            <label for={`target-model-${index}`}>Provider model</label><select
              id={`target-model-${index}`}
              bind:value={target.providerModelId}
              onchange={editor.touch}
              disabled={!editor.canManage}
              >{#if !editor.modelOptions.some((option) => option.id === target.providerModelId)}<option
                  value={target.providerModelId}
                  >{storedTargetLabel(target)} — unavailable</option
                >{/if}{#each editor.modelOptions as option (option.id)}<option
                  value={option.id}>{option.label}</option
                >{/each}</select
            >
          </div>
          <div class="form-field">
            <label for={`priority-${index}`}>Priority</label><input
              id={`priority-${index}`}
              type="number"
              min="0"
              max="32767"
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
              max="1000000"
              bind:value={target.weight}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
          <div class="form-field">
            <label for={`timeout-${index}`}>Attempt timeout (ms)</label><input
              id={`timeout-${index}`}
              type="number"
              min="1"
              bind:value={target.timeoutMs}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
          <div class="form-field">
            <label for={`tags-${index}`}>Selector tags</label><input
              id={`tags-${index}`}
              value={(target.tags ?? []).join(', ')}
              placeholder="fast, tools"
              oninput={(event) => {
                target.tags = splitTags(event.currentTarget.value);
                editor.touch();
              }}
              disabled={!editor.canManage}
            />
          </div>
          <div class="form-field">
            <label for={`shadow-${index}`}>Shadow sample</label><input
              id={`shadow-${index}`}
              type="number"
              min="0"
              max="1"
              step="0.01"
              placeholder="Serves callers"
              value={target.shadowSampleRate ?? ''}
              oninput={(event) => {
                const value = event.currentTarget.value;
                target.shadowSampleRate = value === '' ? null : Number(value);
                editor.touch();
              }}
              disabled={!editor.canManage}
              aria-describedby={`shadow-help-${index}`}
            />
            <small id={`shadow-help-${index}`}
              >Mirror this share of requests here; leave empty to serve.</small
            >
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
          class:warning={targetUnavailable(target) ||
            missingTargetOperations(
              target,
              editor.modelOptions,
              editor.operations
            ).length > 0}
          class="target-eligibility"
        >
          {#if targetUnavailable(target)}<span
              ><strong>Unavailable:</strong> the connection was disabled, or this
              model left its activated revision. The target stays in the route until
              you remove it.</span
            >{/if}
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
        {#if notice}
          <p class="target-lifecycle" class:danger={notice.tone === 'danger'}>
            <strong
              >{notice.tone === 'danger' ? 'Retired:' : 'Retiring:'}</strong
            >
            {notice.text}
          </p>{/if}
      </li>
    {/each}
  </ol>
  {#if editor.routeLifecycleWarnings.length}<div
      class="eligibility-warning"
      role="status"
    >
      <strong>Some targets are deprecated or retiring.</strong><span
        >{editor.routeLifecycleWarnings
          .map((warning) => `${warning.label}: ${warning.notice.text}`)
          .join(' ')} Add a replacement target before the vendor retires them.</span
      >
    </div>{/if}
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
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
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
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .target-number {
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    margin-bottom: 0.35rem;
    border: 1px solid var(--border);
    border-radius: 50%;
    background: transparent;
    color: var(--foreground);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
  }
  .target-fields {
    display: grid;
    grid-column: 2;
    grid-template-columns: minmax(12rem, 2fr) repeat(3, minmax(7rem, 1fr));
    gap: 0.6rem;
  }
  .target-fields small {
    color: var(--foreground-subtle);
    font-size: var(--text-caption);
  }
  .remove-target {
    grid-column: 3;
    grid-row: 1;
    width: 2.5rem;
    height: 2.5rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--danger);
    font-size: 1.3rem;
  }
  .target-eligibility {
    display: grid;
    grid-column: 2 / -1;
    gap: 0.2rem;
    color: var(--success);
    font-size: var(--text-caption);
  }
  .target-eligibility.warning {
    color: var(--warning);
  }
  .target-lifecycle {
    grid-column: 1 / -1;
    margin: 0;
    color: var(--warning);
    font-size: var(--text-body-sm);
  }
  .target-lifecycle.danger {
    color: var(--danger);
  }
  .eligibility-warning {
    display: grid;
    gap: 0.2rem;
    margin-top: 0.75rem;
    padding: 0.75rem;
    border: 1px solid var(--warning);
    border-radius: var(--radius-control);
    background: var(--warning-soft);
    color: var(--foreground);
    font-size: 0.78rem;
  }
  .eligibility-warning strong {
    color: var(--warning);
  }

  .compact {
    min-height: 6rem;
  }
  .compact a {
    color: var(--foreground);
    font-weight: 400;
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition: text-decoration-color var(--motion);
  }
  .compact a:hover {
    text-decoration-color: currentColor;
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
