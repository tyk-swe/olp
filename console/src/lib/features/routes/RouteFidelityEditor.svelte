<script lang="ts">
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
  import type { components } from '$lib/api/schema';
  type Mode = components['schemas']['RouteFidelity']['mode'];
  let { editor }: { editor: RouteDraftEditorState } = $props();
  const saved = $derived(editor.draft.data?.fidelity?.mode ?? null);
  const changed = $derived(
    !editor.isNew && (editor.fidelity?.mode ?? null) !== saved
  );
  const conflict = $derived(
    editor.fidelity?.mode === 'strict' &&
      editor.policyRules.some((rule) => rule.action === 'redact')
  );
  function change(value: string) {
    editor.fidelity = value ? { mode: value as Mode } : null;
    editor.touch();
  }
</script>

<section class="card fidelity-editor" aria-labelledby="route-fidelity-heading">
  <p class="eyebrow">Interaction contract</p>
  <h2 id="route-fidelity-heading">Route fidelity</h2>
  <div class="form-field">
    <label for="route-fidelity">Fidelity mode</label>
    <select
      id="route-fidelity"
      value={editor.fidelity?.mode ?? ''}
      disabled={!editor.canManage || Boolean(editor.busy)}
      onchange={(event) => change(event.currentTarget.value)}
      aria-describedby="route-fidelity-help"
    >
      {#if !editor.isNew && !saved}<option value=""
          >Existing legacy behavior · no explicit contract</option
        >{/if}
      <option value="strict">Strict · preserve the admitted interaction</option>
      <option value="transformed"
        >Transformed · intentional semantic changes</option
      >
      <option value="legacy">Legacy · existing compatibility behavior</option>
    </select>
    <small id="route-fidelity-help"
      >New explicit contracts default to strict. Existing omitted contracts stay
      legacy until you select and activate a change. Native identity and
      qualified translation describe individual plans.</small
    >
  </div>
  {#if changed}<p class="migration-note" role="status">
      Migration selected: {saved ?? 'implicit legacy'} → {editor.fidelity
        ?.mode ?? 'implicit legacy'}. Save and validate the draft before
      activation; the published revision remains the serving contract.
    </p>{/if}
  {#if editor.fidelity?.mode === 'strict'}<p class="muted">
      Strict targets require versioned provider profiles and qualified
      operations and clients. Semantic redaction is incompatible with this
      contract.
    </p>{/if}
  {#if conflict}<p class="inline-problem" role="alert">
      This draft combines strict fidelity with redaction. Use blocking rules or
      deliberately choose transformed fidelity before activation.
    </p>{/if}
</section>

<style>
  .fidelity-editor {
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 1rem;
    font-size: 1rem;
    font-weight: 500;
  }
  .muted,
  .migration-note {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .migration-note {
    padding-left: 0.75rem;
    border-left: 3px solid var(--warning);
  }
</style>
