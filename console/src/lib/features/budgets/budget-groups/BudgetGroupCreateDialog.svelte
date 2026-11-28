<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import ProjectScopeField from '#lib/domain/projects/ProjectScopeField.svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { validRouteSlug } from '#lib/routeIdentity.js';
  import { limitsError } from '../limitForm';
  import type { createBudgetGroup } from './api.js';

  type BudgetGroupInput = Parameters<typeof createBudgetGroup>[0];

  let {
    busy,
    error = $bindable(''),
    onClose,
    onSubmit
  }: {
    busy: boolean;
    error: string;
    onClose: () => void;
    onSubmit: (input: BudgetGroupInput) => Promise<boolean>;
  } = $props();

  let createName = $state('');
  let createProjectId = $state('');
  let createBasis = $state('cost');
  let createDaily = $state('');
  let createMonthly = $state('');
  let createWeekly = $state('');
  let createTemplate = $state('');
  const createRates = $state({
    requests_per_minute: '',
    tokens_per_minute: '',
    max_concurrency: ''
  });
  const rateFields = [
    'requests_per_minute',
    'tokens_per_minute',
    'max_concurrency'
  ] as const;

  usePrincipalLifetime(() => {
    createName =
      createProjectId =
      createDaily =
      createMonthly =
      createWeekly =
      createTemplate =
        '';
    for (const field of rateFields) createRates[field] = '';
  });

  function optionalDecimal(value: string): string | null {
    return value.trim() || null;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !createName.trim()) return;
    if (
      !createTemplate.trim() &&
      !Object.values(createRates).some(Boolean) &&
      !createDaily.trim() &&
      !createMonthly.trim() &&
      !createWeekly.trim()
    ) {
      error = 'Set a limit or reference a project template.';
      return;
    }
    const validation =
      limitsError({
        ...createRates,
        cost_basis: createBasis as 'cost' | 'charge',
        daily_cost_limit: createDaily,
        weekly_cost_limit: createWeekly,
        monthly_cost_limit: createMonthly
      }) ||
      (createTemplate &&
      (!createProjectId || !validRouteSlug(createTemplate.trim()))
        ? 'Choose a project and a valid template name.'
        : '');
    if (validation) {
      error = validation;
      return;
    }
    const saved = await onSubmit({
      name: createName.trim(),
      cost_basis: createBasis as 'cost' | 'charge',
      project_id: createProjectId || null,
      daily_cost_limit: optionalDecimal(createDaily),
      weekly_cost_limit: optionalDecimal(createWeekly),
      monthly_cost_limit: optionalDecimal(createMonthly),
      limit_template: createTemplate.trim() || null,
      requests_per_minute: createRates.requests_per_minute
        ? Number(createRates.requests_per_minute)
        : null,
      tokens_per_minute: createRates.tokens_per_minute
        ? Number(createRates.tokens_per_minute)
        : null,
      max_concurrency: createRates.max_concurrency
        ? Number(createRates.max_concurrency)
        : null
    });
    if (!saved) return;
    createName = '';
    createTemplate = '';
    for (const f of rateFields) createRates[f] = '';
    createDaily = '';
    createMonthly = '';
    createWeekly = '';
    onClose();
  }
</script>

<Dialog
  title="New budget group"
  description="Set at least one limit, or reference a project limit template."
  placement="side"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form class="create-form" onsubmit={submit}>
    <fieldset class="submission-fields" disabled={busy}>
      <div class="form-grid">
        <div class="form-field">
          <label for="group-name">Group name</label><input
            id="group-name"
            bind:value={createName}
            required
          />
        </div>
        <ProjectScopeField id="group-project" bind:value={createProjectId} />
        <div class="form-field">
          <label for="group-template">Limit template</label><input
            id="group-template"
            bind:value={createTemplate}
            maxlength="100"
          />
        </div>
        {#each rateFields as field (field)}<div class="form-field">
            <label for={`group-${field}`}>
              {field.replaceAll('_', ' ')}
            </label><input
              id={`group-${field}`}
              bind:value={createRates[field]}
              inputmode="numeric"
            />
          </div>{/each}
        <div class="form-field">
          <label for="group-daily">Daily cost limit</label><input
            id="group-daily"
            inputmode="decimal"
            placeholder="10.00"
            bind:value={createDaily}
          />
        </div>
        <div class="form-field">
          <label for="group-weekly">Weekly cost limit</label><input
            id="group-weekly"
            inputmode="decimal"
            placeholder="100.00"
            bind:value={createWeekly}
          />
        </div>
        <div class="form-field">
          <label for="group-monthly">Monthly cost limit</label><input
            id="group-monthly"
            inputmode="decimal"
            placeholder="100.00"
            bind:value={createMonthly}
          />
        </div>
        <div class="form-field">
          <label for="group-basis">Budget basis</label><select
            id="group-basis"
            bind:value={createBasis}
          >
            <option value="cost">Provider cost</option><option value="charge">
              Consumer charge
            </option>
          </select>
        </div>
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={busy || !createName.trim()}
        >
          {busy ? 'Creating…' : 'Create group'}
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
