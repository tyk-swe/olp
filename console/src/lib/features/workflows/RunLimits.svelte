<script lang="ts">
  import type { components } from '$lib/api/schema';

  type Limits = components['schemas']['WorkflowRunLimits'];
  type IntegerField = Exclude<keyof Limits, 'max_run_cost'>;
  let {
    value = null,
    disabled = false,
    prefix,
    onChange
  }: {
    value?: Limits | null;
    disabled?: boolean;
    prefix: string;
    onChange: (next: Limits | null) => void;
  } = $props();

  const integerFields: {
    key: IntegerField;
    label: string;
    min: number;
    max: number;
  }[] = [
    {
      key: 'max_iterations',
      label: 'Maximum model iterations',
      min: 1,
      max: 32
    },
    { key: 'max_tool_calls', label: 'Maximum tool calls', min: 0, max: 128 },
    {
      key: 'max_duration_seconds',
      label: 'Maximum run duration in seconds',
      min: 1,
      max: 600
    },
    {
      key: 'max_concurrent_runs',
      label: 'Maximum concurrent runs per key',
      min: 1,
      max: 2147483647
    }
  ];

  function update(key: keyof Limits, raw: string) {
    const next = {
      ...value,
      [key]: raw === '' ? undefined : key === 'max_run_cost' ? raw : Number(raw)
    };
    onChange(
      Object.values(next).some((entry) => entry !== undefined) ? next : null
    );
  }
</script>

<fieldset {disabled}>
  <legend>Workflow run limits</legend>
  <small
    >Leave a field empty to use the applicable default or another configured
    limit.</small
  >
  {#each integerFields as field (field.key)}
    <div class="form-field">
      <label for={`${prefix}-${field.key}`}>{field.label}</label>
      <input
        id={`${prefix}-${field.key}`}
        type="number"
        min={field.min}
        max={field.max}
        step="1"
        value={value?.[field.key] ?? ''}
        oninput={(event) => update(field.key, event.currentTarget.value)}
      />
    </div>
  {/each}
  <div class="form-field">
    <label for={`${prefix}-max_run_cost`}>Maximum run cost</label>
    <input
      id={`${prefix}-max_run_cost`}
      type="text"
      inputmode="decimal"
      value={value?.max_run_cost ?? ''}
      oninput={(event) => update('max_run_cost', event.currentTarget.value)}
    />
    <small>Uses the installation pricing currency.</small>
  </div>
</fieldset>
