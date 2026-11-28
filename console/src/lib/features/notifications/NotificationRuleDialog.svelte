<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { ApiKey } from '#lib/domain/api-keys/api.js';
  import ProjectScopeField from '#lib/domain/projects/ProjectScopeField.svelte';
  import { notificationEvents, newRuleDraft } from './ruleDraft';
  import type { RuleDraft, RuleField } from './ruleDraft';
  import type { BudgetGroup } from '#lib/domain/budget-groups/api.js';
  import type { NotificationDestination } from './api';
  import { ruleSubjectLabels, ruleWindowLabels } from './presentation';

  let {
    draft = $bindable(),
    busy,
    error,
    errors,
    installationWide,
    subjectOptions,
    destinationOptions,
    onClose,
    onSubmit
  }: {
    draft: RuleDraft;
    busy: boolean;
    error: string;
    errors: FieldErrors<RuleField>;
    installationWide: boolean;
    subjectOptions: (ApiKey | BudgetGroup)[];
    destinationOptions: NotificationDestination[];
    onClose: () => void;
    onSubmit: (event: SubmitEvent & { currentTarget: HTMLFormElement }) => void;
  } = $props();

  const ruleEvent = $derived(draft.event);
  const watchesBudget = $derived(ruleEvent === 'budget.threshold');
  const watchesKey = $derived(ruleEvent === 'key.expiring');
  const genericRule = $derived(
    !watchesBudget && ruleEvent !== 'provider.grant.lapsed'
  );
  const ruleProjectScoped = $derived(
    notificationEvents[ruleEvent].scope === 'project'
  );
</script>

<Dialog
  title="New alert rule"
  description="Send an alert to a destination when something happens, such as a budget threshold."
  placement="side"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form class="create-form" onsubmit={onSubmit}>
    <fieldset class="submission-fields" disabled={busy}>
      <div class="form-grid">
        <FormField label="Rule name" error={errors.name}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={draft.name} required />
          {/snippet}
        </FormField>
        <div class="form-field">
          <label for="rule-event">Event</label><select
            id="rule-event"
            value={ruleEvent}
            onchange={(event) => {
              const selected = Object.values(notificationEvents).find(
                (definition) => definition.event === event.currentTarget.value
              );
              if (selected) draft = newRuleDraft(selected.event, draft);
            }}
          >
            {#each Object.values(notificationEvents) as definition (definition.event)}<option
                value={definition.event}
                disabled={!installationWide && definition.scope !== 'project'}
              >
                {definition.label}
              </option>{/each}
          </select>
        </div>
        {#if ruleProjectScoped}
          <ProjectScopeField
            id="rule-project"
            bind:value={draft.projectId}
            unassigned={installationWide && draft.event !== 'spend.spike'}
          />
        {/if}
        {#if draft.event === 'budget.threshold' || draft.event === 'key.expiring'}
          {@const watched = draft}
          <div class="form-field">
            <label for="rule-subject-kind">Subject</label><select
              id="rule-subject-kind"
              disabled={watchesKey}
              bind:value={draft.subjectKind}
            >
              {#each Object.entries(ruleSubjectLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          </div>
          <FormField
            label={draft.subjectKind === 'api_key' ? 'API key' : 'Budget group'}
            error={errors.subjectId}
          >
            {#snippet control(attributes)}
              <select {...attributes} bind:value={watched.subjectId} required>
                <option value="" disabled>
                  Choose a subject
                </option>{#each subjectOptions as subject (subject.id)}<option
                    value={subject.id}
                  >
                    {subject.name}
                  </option>{/each}
              </select>
            {/snippet}
          </FormField>
          {#if draft.event === 'budget.threshold'}
            {@const budget = draft}
            <div class="form-field">
              <label for="rule-window">Window</label><select
                id="rule-window"
                bind:value={draft.window}
              >
                {#each Object.entries(ruleWindowLabels) as [value, label] (value)}
                  <option {value}>{label}</option>
                {/each}
              </select>
            </div>
            <FormField label="Threshold %" error={errors.threshold}>
              {#snippet control(attributes)}
                <input
                  {...attributes}
                  inputmode="numeric"
                  bind:value={budget.threshold}
                  required
                />
              {/snippet}
            </FormField>
          {/if}
        {/if}
        {#each notificationEvents[ruleEvent].fields as field (field.key)}
          <FormField label={field.label} error={errors[field.key]}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                inputmode={field.inputmode}
                value={draft.values[field.key] ?? ''}
                oninput={(e) =>
                  (draft.values = {
                    ...draft.values,
                    [field.key]: e.currentTarget.value
                  })}
                placeholder="Default"
              />
            {/snippet}
          </FormField>
        {/each}
        {#if draft.event === 'route.latency'}
          <div class="form-field">
            <label for="rule-metric">Metric</label><select
              id="rule-metric"
              bind:value={draft.metric}
            >
              <option value="ttft">Time to first token</option><option
                value="latency"
              >
                Total latency
              </option>
            </select>
          </div>
        {/if}
        {#if draft.event === 'report.spend'}
          <div class="form-field">
            <label for="rule-period">Period</label><select
              id="rule-period"
              bind:value={draft.period}
            >
              <option value="daily">Daily</option><option value="weekly">
                Weekly
              </option><option value="monthly">Monthly</option>
            </select>
          </div>
        {/if}
        <FormField label="Destination" error={errors.destinationId}>
          {#snippet control(attributes)}
            <select {...attributes} bind:value={draft.destinationId} required>
              <option value="" disabled>
                Choose a destination
              </option>{#each destinationOptions as destination (destination.id)}<option
                  value={destination.id}
                >
                  {destination.name}
                </option>{/each}
            </select>
          {/snippet}
        </FormField>
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={busy ||
            !draft.name.trim() ||
            ('subjectId' in draft && !draft.subjectId) ||
            !draft.destinationId}
        >
          {busy ? 'Creating…' : 'Create rule'}
        </button>
      </div>
      <p class="section-help">
        {#if draft.event === 'budget.threshold'}A delivery is sent when a
          subject's accrued spend crosses the threshold within the window.{:else if watchesKey}Sent
          once when the key expires or reaches its rotation date within the lead
          time.{:else if draft.event === 'spend.spike'}Sent once when a complete
          UTC day of project spend jumps above its recent baseline by both
          thresholds.{:else if genericRule}Metadata-only trigger and recovery
          deliveries; cooldown repeats share the incident key.{:else}A grant
          lapses when its provider plugin can no longer refresh it.{/if}
      </p>
    </fieldset>
  </form>
</Dialog>
