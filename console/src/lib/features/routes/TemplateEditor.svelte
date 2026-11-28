<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import FormField from '#lib/components/FormField.svelte';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import {
    guardUnsavedChanges,
    hasChanges
  } from '#lib/editing/unsavedChanges.js';
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import { fidelityOptions } from './routeFidelity';
  import { saveTemplate, type RouteTemplate } from './api';
  import {
    templateDraft,
    templateServerFields,
    validateTemplate,
    type TemplateField
  } from './templateDraft';
  import { routeKeys } from '#lib/domain/routes/routeKeys.js';
  let {
    current,
    projects,
    saved,
    reload
  }: {
    current: RouteTemplate | null;
    projects: { id: string; name: string }[];
    reload?: () => void;
    saved: (value: RouteTemplate) => Promise<void>;
  } = $props();
  const initial = untrack(() => current);
  let baseline = $state(initial);
  const draft = $state(templateDraft(initial));
  let clean = $state.raw(templateDraft(initial));
  const client = useQueryClient();
  const action = useMutation<TemplateField>(() =>
    client.invalidateQueries({ queryKey: routeKeys.templates() })
  );
  const errors = $derived(action.fieldErrors);

  async function submit(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    const checked = validateTemplate(draft);
    if (!action.check(checked)) return focusFormError(form);
    const accepted = await action.submit(
      { current: baseline, body: checked.value },
      (input, requestId) => saveTemplate(input.current, input.body, requestId),
      async (value) => {
        baseline = value;
        clean = $state.snapshot(draft);
        await saved(value);
      },
      { serverFields: templateServerFields }
    );
    if (!accepted) await focusFormError(form);
  }
  export function isDirty() {
    return hasChanges(clean, $state.snapshot(draft));
  }
  guardUnsavedChanges(isDirty);
</script>

<form class="card form-card" onsubmit={submit}>
  <h2>{current ? 'Edit template' : 'Create template'}</h2>
  <FormField label="Name" error={errors.name}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        required
        pattern={'[a-z0-9][a-z0-9\\-]{0,62}'}
        bind:value={draft.name}
      />
    {/snippet}
  </FormField>
  <FormField label="Project" error={errors.project}>
    {#snippet control(attributes)}
      <select
        {...attributes}
        disabled={Boolean(baseline)}
        bind:value={draft.project}
      >
        <option value="">
          Installation
        </option>{#each projects as item (item.id)}<option value={item.id}>
            {item.name}
          </option>{/each}
      </select>
    {/snippet}
  </FormField>
  <FormField label="Provider selector" error={errors.selector}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        required
        placeholder="provider UUID or vendor ID, prefixed with provider: or vendor:"
        bind:value={draft.selector}
      />
    {/snippet}
  </FormField>
  <FormField label="Model filter" error={errors.filter}>
    {#snippet control(attributes)}
      <input {...attributes} required bind:value={draft.filter} />
    {/snippet}
  </FormField>
  <FormField label="Route slug pattern" error={errors.slug}>
    {#snippet control(attributes)}
      <input {...attributes} required bind:value={draft.slug} />
    {/snippet}
  </FormField>
  <p class="inline-status">
    Use {'{model}'} and optionally {'{vendor}'} in the slug pattern
  </p>
  <FormField label="Timeout in milliseconds" error={errors.timeout}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        type="number"
        min="1"
        max="3600000"
        required
        bind:value={draft.timeout}
      />
    {/snippet}
  </FormField>
  <FormField label="Maximum attempts" error={errors.attempts}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        type="number"
        min="1"
        max="32767"
        required
        bind:value={draft.attempts}
      />
    {/snippet}
  </FormField>
  <FormField label="Fidelity" error={errors.fidelity}>
    {#snippet control(attributes)}
      <select {...attributes} bind:value={draft.fidelity}>
        {#each fidelityOptions as option (option.value)}<option
            value={option.value}
          >
            {option.label}
          </option>{/each}
      </select>
    {/snippet}
  </FormField>
  <FormField label="Routing policy JSON" error={errors.policy} full>
    {#snippet control(attributes)}
      <textarea
        {...attributes}
        bind:value={draft.policy}
        placeholder="Leave blank to inherit"></textarea>
    {/snippet}
  </FormField>
  <label class="check-field">
    <input type="checkbox" bind:checked={draft.publish} />Automatically publish
    generated routes
  </label>
  {#if draft.publish}<p class="inline-status">
      Applying this template or activating a matching provider can publish
      routes immediately
    </p>{/if}
  <div class="page-actions">
    <button class="button button-primary" disabled={action.busy}>
      Save template
    </button>
  </div>
  {#if reload && current?.etag !== baseline?.etag}
    <ConflictNotice notice="newer" onReload={reload} disabled={action.busy} />
  {/if}
  {#if action.error}<p
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
    >
      {action.error}
    </p>{/if}
</form>
