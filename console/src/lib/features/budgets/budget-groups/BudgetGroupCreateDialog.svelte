<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import ProjectScopeField from '#lib/domain/projects/ProjectScopeField.svelte';
  import type { useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { costBasisLabels } from '../limitForm.js';
  import type { createBudgetGroup } from './api.js';
  import {
    budgetGroupDraft,
    validateNewBudgetGroup,
    type BudgetGroupField
  } from './budgetGroupDraft';

  type BudgetGroupInput = Parameters<typeof createBudgetGroup>[0];

  let {
    action,
    onClose,
    onSubmit
  }: {
    action: ReturnType<typeof useMutation<BudgetGroupField>>;
    onClose: () => void;
    onSubmit: (input: BudgetGroupInput) => Promise<boolean>;
  } = $props();

  let draft = $state(budgetGroupDraft());
  const rateFields = [
    'requests_per_minute',
    'tokens_per_minute',
    'max_concurrency'
  ] as const;
  const costFields = [
    { key: 'daily_cost_limit', label: 'Daily cost limit', example: '10.00' },
    { key: 'weekly_cost_limit', label: 'Weekly cost limit', example: '100.00' },
    {
      key: 'monthly_cost_limit',
      label: 'Monthly cost limit',
      example: '100.00'
    }
  ] as const;

  usePrincipalLifetime(() => {
    draft = budgetGroupDraft();
  });

  async function submit(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    if (action.busy || !draft.name.trim()) return;
    const checked = validateNewBudgetGroup($state.snapshot(draft));
    if (!action.check(checked)) return focusFormError(form);
    if (await onSubmit(checked.value)) return onClose();
    await focusFormError(form);
  }
</script>

<Dialog
  title="New budget group"
  description="Set at least one limit, or reference a project limit template."
  placement="side"
  {onClose}
>
  {#if action.error}<Callout tone="danger">{action.error}</Callout>{/if}
  <form class="create-form" onsubmit={submit}>
    <fieldset class="submission-fields" disabled={action.busy}>
      <div class="form-grid">
        <FormField label="Group name" error={action.fieldErrors.name}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={draft.name} required />
          {/snippet}
        </FormField>
        <ProjectScopeField id="group-project" bind:value={draft.project_id} />
        <FormField
          label="Limit template"
          error={action.fieldErrors.limit_template}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              bind:value={draft.limit_template}
              maxlength="100"
            />
          {/snippet}
        </FormField>
        {#each rateFields as field (field)}<FormField
            label={field.replaceAll('_', ' ')}
            error={action.fieldErrors[field]}
          >
            {#snippet control(attributes)}
              <input
                {...attributes}
                bind:value={draft[field]}
                inputmode="numeric"
              />
            {/snippet}
          </FormField>{/each}
        {#each costFields as { key, label, example } (key)}<FormField
            {label}
            error={action.fieldErrors[key]}
          >
            {#snippet control(attributes)}
              <input
                {...attributes}
                inputmode="decimal"
                placeholder={example}
                bind:value={draft[key]}
              />
            {/snippet}
          </FormField>{/each}
        <FormField label="Budget basis" error={action.fieldErrors.cost_basis}>
          {#snippet control(attributes)}
            <select {...attributes} bind:value={draft.cost_basis}>
              {#each Object.entries(costBasisLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          {/snippet}
        </FormField>
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={action.busy || !draft.name.trim()}
        >
          {action.busy ? 'Creating…' : 'Create group'}
        </button>
      </div>
    </fieldset>
  </form>
</Dialog>

<style>
  .submission-fields {
    display: grid;
    gap: inherit;
    border: 0;
    padding: 0;
    margin: 0;
    min-width: 0;
  }
  .create-form {
    display: grid;
    gap: 1rem;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .form-actions {
    display: flex;
    justify-content: flex-end;
  }
</style>
