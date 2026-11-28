<script lang="ts">
  import RuleSilenceEditor from './RuleSilenceEditor.svelte';
  import { notificationEvents } from './ruleDraft';
  import type { NotificationDestination, NotificationRule } from './api';
  import type { RuleConfigurationDraft, RuleField } from './ruleDraft';
  import type { FieldErrors } from '#lib/editing/validation.js';

  let {
    rule,
    name = $bindable(''),
    destinationId = $bindable(''),
    configuration = $bindable(),
    destinations,
    busy,
    errors,
    editingRule,
    onSave,
    onCancel,
    onSilenceSaved,
    subjectLabel,
    windowLabel
  }: {
    rule: NotificationRule;
    name: string;
    destinationId: string;
    configuration: RuleConfigurationDraft;
    destinations: NotificationDestination[];
    busy: boolean;
    errors: FieldErrors<RuleField>;
    editingRule: NotificationRule | null;
    onSave: () => void;
    onCancel: () => void;
    onSilenceSaved: (value: NotificationRule) => Promise<void>;
    subjectLabel: (rule: NotificationRule) => string;
    windowLabel: (windowKind: NotificationRule['window_kind']) => string;
  } = $props();

  const id = $props.id();
  function reported(field: RuleField) {
    return errors[field]
      ? {
          'aria-invalid': 'true' as const,
          'aria-describedby': `${id}-${field}`
        }
      : {};
  }
</script>

{#snippet problemAt(field: RuleField)}
  {#if errors[field]}<p class="field-error" id="{id}-{field}">
      {errors[field]}
    </p>{/if}
{/snippet}

<tr>
  <td>
    <input
      {...reported('name')}
      aria-label="Rule name"
      bind:value={name}
      disabled={busy}
      required
    />{@render problemAt('name')}<br /><small>
      {rule.project_name ?? 'Installation-wide'}
    </small>
  </td><td>{notificationEvents[rule.event].label}</td><td>
    {subjectLabel(rule)}
  </td><td>{windowLabel(rule.window_kind)}</td><td>
    <div class="form-grid">
      {#each notificationEvents[rule.event].fields as field (field.key)}
        <input
          {...reported(field.key)}
          aria-label={field.label}
          inputmode={field.inputmode}
          value={configuration.values[field.key] ?? ''}
          disabled={busy}
          oninput={(e) =>
            (configuration.values = {
              ...configuration.values,
              [field.key]: e.currentTarget.value
            })}
          placeholder={`${field.key} (default)`}
        />
      {/each}
      {#if configuration.event === 'route.latency'}<select
          aria-label="Metric"
          bind:value={configuration.metric}
          disabled={busy}
        >
          <option value="ttft">Time to first token</option><option
            value="latency"
          >
            Total latency
          </option>
        </select>{/if}
      {#if configuration.event === 'report.spend'}<select
          aria-label="Period"
          bind:value={configuration.period}
          disabled={busy}
        >
          <option value="daily">Daily</option><option value="weekly">
            Weekly
          </option><option value="monthly">Monthly</option>
        </select>{/if}
    </div>
    {#each notificationEvents[rule.event].fields as field (field.key)}
      {@render problemAt(field.key)}
    {/each}
  </td><td>
    <select
      {...reported('destinationId')}
      aria-label="Destination"
      bind:value={destinationId}
      disabled={busy}
    >
      {#each destinations.filter((d) => (d.project_id ?? null) === (rule.project_id ?? null)) as destination (destination.id)}<option
          value={destination.id}
        >
          {destination.name}
        </option>{/each}
    </select>{@render problemAt('destinationId')}
  </td><td>{rule.enabled ? 'Yes' : 'No'}</td><td>
    <button
      class="text-button"
      type="button"
      disabled={busy}
      onclick={() => onSave()}
    >
      {busy ? 'Saving…' : 'Save'}
    </button><button
      class="text-button"
      type="button"
      disabled={busy}
      onclick={onCancel}
    >
      Cancel
    </button>
  </td>
</tr>
{#if editingRule?.event === 'route.latency' && editingRule.project_id && destinations.some((destination) => destination.id === editingRule?.destination_id && destination.type === 'webhook')}
  <tr>
    <td colspan="8">
      <RuleSilenceEditor
        rule={editingRule}
        disabled={busy}
        saved={onSilenceSaved}
      />
    </td>
  </tr>
{/if}
