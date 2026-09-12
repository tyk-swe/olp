<script lang="ts">
  import { parseJsonObject } from '$lib/forms/json';
  import { parseManualModelNames } from '$lib/features/providers/providerEditor';
  let {
    value = $bindable('{}'),
    id = 'routing-preferences',
    disabled = false,
    constraintsOnly = false,
    onChange = () => {}
  }: {
    value?: string;
    id?: string;
    disabled?: boolean;
    constraintsOnly?: boolean;
    onChange?: (value: string) => void;
  } = $props();
  function read(): Record<string, unknown> {
    return parseJsonObject(value) ?? {};
  }
  const current = $derived.by(() => {
    void value;
    return read();
  });
  function set(key: string, field: unknown) {
    const next = read();
    if (field === undefined) delete next[key];
    else next[key] = field;
    value = JSON.stringify(next, null, 2);
    onChange(value);
  }
  function list(key: string, field: string) {
    set(key, field.trim() ? parseManualModelNames(field) : undefined);
  }
  function joined(key: string) {
    const field = current[key];
    return Array.isArray(field) ? field.join(', ') : '';
  }
</script>

<fieldset {disabled} class="preferences">
  <legend
    >{constraintsOnly ? 'Required constraints' : 'Routing preferences'}</legend
  >
  {#if !constraintsOnly}
    <label for="{id}-strategy">Strategy</label><select
      id="{id}-strategy"
      value={String(current.strategy ?? '')}
      onchange={(e) => set('strategy', e.currentTarget.value || undefined)}
      ><option value="">Use configured default</option><option value="weighted"
        >Weighted</option
      ><option value="price">Price</option><option value="latency"
        >Latency</option
      ><option value="throughput">Throughput</option></select
    >
  {/if}
  {#each [{ key: 'only', label: 'Only these vendors or connections' }, { key: 'ignore', label: 'Exclude vendors or connections' }, ...(constraintsOnly ? [] : [{ key: 'order', label: 'Preferred order' }]), { key: 'regions', label: 'Allowed regions' }] as field (field.key)}
    <label for="{id}-{field.key}">{field.label}</label><input
      id="{id}-{field.key}"
      value={joined(field.key)}
      placeholder={field.key === 'regions'
        ? 'us-east, eu-west'
        : 'vendor:deepseek, provider:UUID'}
      oninput={(e) => list(field.key, e.currentTarget.value)}
    />
  {/each}
  {#if !constraintsOnly}<label class="check"
      ><input
        type="checkbox"
        checked={current.allow_fallbacks === false}
        onchange={(e) =>
          set('allow_fallbacks', e.currentTarget.checked ? false : undefined)}
      />Disable fallback</label
    >{/if}
  {#each [{ key: 'deny_data_collection', label: 'Require no data collection' }, { key: 'require_zero_data_retention', label: 'Require zero data retention' }, { key: 'require_parameters', label: 'Require declared parameter support' }] as field (field.key)}
    <label class="check"
      ><input
        type="checkbox"
        checked={current[field.key] === true}
        onchange={(e) => set(field.key, e.currentTarget.checked || undefined)}
      />{field.label}</label
    >
  {/each}
  <details>
    <summary>Advanced preferences</summary><label for="{id}-json"
      >Preferences JSON</label
    ><textarea
      id="{id}-json"
      bind:value
      oninput={() => onChange(value)}
      rows="8"
      spellcheck="false"></textarea>
    <p>
      Price ceilings use max_price.input_per_million, output_per_million, and
      unit_price as decimal strings. Performance preferences use
      preferred_max_latency_ms and preferred_min_throughput.
    </p>
  </details>
</fieldset>

<style>
  fieldset {
    margin: 1rem 0;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-card);
    padding: 1rem;
  }
  legend {
    font-weight: 500;
  }
  label {
    display: block;
    margin: 0.65rem 0 0.25rem;
    font-size: 0.85rem;
  }
  /* These controls sit outside .form-field, so the shared control recipe is
     restated here. */
  input:not([type='checkbox']),
  select,
  textarea {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
  }
  input:not([type='checkbox']):hover,
  select:hover,
  textarea:hover {
    border-color: var(--border-strong);
  }
  input::placeholder {
    color: var(--foreground-muted);
  }
  .check {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .check input {
    margin: 0;
  }
  details {
    margin-top: 1rem;
  }
  summary {
    cursor: pointer;
  }
  textarea {
    min-height: 7rem;
    font-family: var(--font-mono);
    resize: vertical;
  }
  p {
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
</style>
