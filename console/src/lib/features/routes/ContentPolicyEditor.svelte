<script lang="ts">
  import type { RouteDraftEditorState } from '$lib/features/routes/routeDraftEditor.svelte';
  let { editor }: { editor: RouteDraftEditorState } = $props();
</script>

<section class="card editor policy" aria-labelledby="content-policy-heading">
  <p class="eyebrow">Content policy</p>
  <h2 id="content-policy-heading">Inspect request and response text</h2>
  <p class="policy-note">
    Rules are RE2 expressions applied in order. Input rules run before any
    provider call; output rules apply to buffered unary responses only, so a
    policy with output rules rejects streaming requests. Matched text is
    inspected transiently and is never logged or stored — request history
    records only rule ids and outcomes.
  </p>
  {#if editor.outputPolicyActive}<p class="policy-warning" role="status">
      Output rules make streaming unavailable on this route.
    </p>{/if}
  {#if !editor.policyRules.length}<p class="policy-empty">
      No content policy — request and response text passes through uninspected.
    </p>{/if}
  {#each editor.policyRules as rule, index (index)}
    <div class="policy-rule">
      <div class="form-grid">
        <div class="form-field">
          <label for={`policy-id-${index}`}>Rule id</label><input
            id={`policy-id-${index}`}
            autocomplete="off"
            bind:value={rule.id}
            oninput={editor.touch}
            disabled={!editor.canManage}
          />
        </div>
        <div class="form-field">
          <label for={`policy-phase-${index}`}>Phase</label><select
            id={`policy-phase-${index}`}
            bind:value={rule.phase}
            onchange={editor.touch}
            disabled={!editor.canManage}
            ><option value="input">input</option><option value="output"
              >output</option
            ></select
          >
        </div>
        <div class="form-field">
          <label for={`policy-action-${index}`}>Action</label><select
            id={`policy-action-${index}`}
            bind:value={rule.action}
            onchange={editor.touch}
            disabled={!editor.canManage}
            ><option value="redact">redact</option><option value="block"
              >block</option
            ></select
          >
        </div>
        <div class="form-field">
          <label for={`policy-pattern-${index}`}>Pattern (RE2)</label><input
            id={`policy-pattern-${index}`}
            class="mono"
            autocomplete="off"
            spellcheck="false"
            bind:value={rule.pattern}
            oninput={editor.touch}
            disabled={!editor.canManage}
          />
        </div>
        {#if rule.action === 'redact'}<div class="form-field">
            <label for={`policy-replacement-${index}`}>Replacement</label><input
              id={`policy-replacement-${index}`}
              autocomplete="off"
              placeholder="[REDACTED]"
              bind:value={rule.replacement}
              oninput={editor.touch}
              disabled={!editor.canManage}
            /><small>Inserted literally; $-patterns are never expanded.</small>
          </div>{/if}
      </div>
      {#if editor.canManage}<button
          class="button button-secondary rule-remove"
          type="button"
          onclick={() => editor.removePolicyRule(index)}>Remove</button
        >{/if}
    </div>
  {/each}
  {#if editor.canManage}<button
      class="button button-secondary"
      type="button"
      onclick={editor.addPolicyRule}>Add rule</button
    >{/if}
</section>

<style>
  .policy {
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .policy-note,
  .policy-empty {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .policy-warning {
    padding: 0.65rem 0.85rem;
    border: 1px solid var(--warning);
    border-radius: var(--radius-control);
    background: var(--warning-soft);
  }
  .policy-rule {
    padding: 0.85rem 0;
    border-top: 1px solid var(--border-hairline);
  }
  .rule-remove {
    margin-top: 0.5rem;
  }
  .mono {
    font-family: var(--font-mono);
  }
  .form-field small {
    display: block;
    margin-top: 0.25rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
