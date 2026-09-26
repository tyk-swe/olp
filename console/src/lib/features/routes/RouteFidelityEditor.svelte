<script lang="ts">
  import type { RouteDraftEditorState } from './routeDraftEditor.svelte';
  import type { components } from '$lib/api/schema';
  type Mode = NonNullable<components['schemas']['RouteFidelity']['mode']>;
  let { editor }: { editor: RouteDraftEditorState } = $props();
  const mode = $derived<Mode>(editor.fidelity.mode ?? 'strict');
  const saved = $derived(editor.draft.data?.fidelity?.mode);
  const changed = $derived(
    !editor.isNew && saved !== undefined && mode !== saved
  );
  const conflict = $derived(
    mode === 'strict' &&
      editor.policyRules.some((rule) => rule.action === 'redact')
  );
  function change(value: string) {
    editor.fidelity = { mode: value as Mode };
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
      value={mode}
      disabled={!editor.canManage || Boolean(editor.busy)}
      onchange={(event) => change(event.currentTarget.value)}
      aria-describedby="route-fidelity-help"
    >
      <option value="strict">Strict · preserve the native invocation</option>
      <option value="transformed"
        >Transformed · translate or redact deliberately</option
      >
    </select>
    <small id="route-fidelity-help"
      >Routes are strict unless you declare them transformed. Native identity
      and qualified translation describe individual plans.</small
    >
  </div>
  {#if changed}<p class="change-note" role="status">
      Draft fidelity: {saved} → {mode}. Activation publishes the change as a new
      revision of this route.
    </p>{/if}
  {#if mode === 'strict'}<p class="muted">
      Strict targets require versioned provider profiles and qualified
      operations and clients. Redaction is incompatible with this contract.
    </p>{:else}<p class="muted">
      Transformed routes may translate between dialects, redact content and use
      providers without a profile.
    </p>{/if}
  {#if conflict}<p class="inline-problem" role="alert">
      This draft combines strict fidelity with redaction. Use blocking rules or
      declare the route transformed before activation.
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
  .change-note {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .change-note {
    padding-left: 0.75rem;
    border-left: 3px solid var(--warning);
  }
</style>
