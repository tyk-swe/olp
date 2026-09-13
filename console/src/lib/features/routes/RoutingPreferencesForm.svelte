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
  /** The price ceiling is one nested object, so it is read and written whole. */
  function ceiling(): Record<string, unknown> {
    const field = current.max_price;
    return field && typeof field === 'object' && !Array.isArray(field)
      ? (field as Record<string, unknown>)
      : {};
  }
  function price(key: string) {
    const field = ceiling()[key];
    return typeof field === 'string' ? field : '';
  }
  function setPrice(key: string, field: string) {
    const next = { ...ceiling() };
    if (field.trim()) next[key] = field.trim();
    else delete next[key];
    set('max_price', Object.keys(next).length ? next : undefined);
  }
  function count(key: string) {
    const field = current[key];
    return typeof field === 'number' ? String(field) : '';
  }
  function setCount(key: string, field: string) {
    const parsed = Number(field.trim());
    set(
      key,
      field.trim() && Number.isInteger(parsed) && parsed > 0
        ? parsed
        : undefined
    );
  }
  const listFields = $derived([
    {
      key: 'only',
      label: 'Only these vendors or connections',
      hint: 'vendor:deepseek, provider:UUID'
    },
    {
      key: 'ignore',
      label: 'Exclude vendors or connections',
      hint: 'vendor:deepseek, provider:UUID'
    },
    ...(constraintsOnly
      ? []
      : [
          {
            key: 'order',
            label: 'Preferred order',
            hint: 'vendor:deepseek, provider:UUID'
          }
        ]),
    { key: 'regions', label: 'Allowed regions', hint: 'us-east, eu-west' },
    { key: 'quantizations', label: 'Allowed quantizations', hint: 'fp8, int4' }
  ]);
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
  {#each listFields as field (field.key)}
    <label for="{id}-{field.key}">{field.label}</label><input
      id="{id}-{field.key}"
      value={joined(field.key)}
      placeholder={field.hint}
      oninput={(e) => list(field.key, e.currentTarget.value)}
    />
  {/each}
  {#if !constraintsOnly}
    <label for="{id}-fallbacks">Fallback attempts</label><select
      id="{id}-fallbacks"
      value={current.allow_fallbacks === true
        ? 'true'
        : current.allow_fallbacks === false
          ? 'false'
          : ''}
      onchange={(e) =>
        set(
          'allow_fallbacks',
          e.currentTarget.value ? e.currentTarget.value === 'true' : undefined
        )}
      ><option value="">Use configured default</option><option value="true"
        >Allow fallback attempts</option
      ><option value="false">Disable fallback</option></select
    >
  {/if}
  {#each [{ key: 'deny_data_collection', label: 'Require no data collection' }, { key: 'require_zero_data_retention', label: 'Require zero data retention' }, { key: 'require_parameters', label: 'Require declared parameter support' }] as field (field.key)}
    <label class="check"
      ><input
        type="checkbox"
        checked={current[field.key] === true}
        onchange={(e) => set(field.key, e.currentTarget.checked || undefined)}
      />{field.label}</label
    >
  {/each}
  <div class="group" role="group" aria-labelledby="{id}-price-heading">
    <p class="group-heading" id="{id}-price-heading">Price ceiling</p>
    {#each [{ key: 'input_per_million', label: 'Max input price per million' }, { key: 'output_per_million', label: 'Max output price per million' }, { key: 'unit_price', label: 'Max unit price' }] as field (field.key)}
      <label for="{id}-{field.key}">{field.label}</label><input
        id="{id}-{field.key}"
        value={price(field.key)}
        inputmode="decimal"
        placeholder="No ceiling"
        oninput={(e) => setPrice(field.key, e.currentTarget.value)}
      />
    {/each}
    <p class="hint">
      Decimal amounts in the model's own currency. A target priced above any
      ceiling you set is excluded.
    </p>
  </div>
  {#if !constraintsOnly}
    <div class="group" role="group" aria-labelledby="{id}-performance-heading">
      <p class="group-heading" id="{id}-performance-heading">Performance</p>
      <label for="{id}-preferred_max_latency_ms"
        >Preferred maximum latency</label
      ><input
        id="{id}-preferred_max_latency_ms"
        value={count('preferred_max_latency_ms')}
        type="number"
        min="1"
        step="1"
        placeholder="No preference"
        oninput={(e) =>
          setCount('preferred_max_latency_ms', e.currentTarget.value)}
      />
      <label for="{id}-preferred_min_throughput"
        >Preferred minimum throughput</label
      ><input
        id="{id}-preferred_min_throughput"
        value={count('preferred_min_throughput')}
        type="number"
        min="1"
        step="1"
        placeholder="No preference"
        oninput={(e) =>
          setCount('preferred_min_throughput', e.currentTarget.value)}
      />
      <p class="hint">
        Latency in milliseconds and throughput in output tokens per second,
        measured from recent traffic. These order the attempts; they never
        exclude a target on their own.
      </p>
    </div>
  {/if}
  <details>
    <summary>Advanced preferences</summary><label for="{id}-json"
      >Preferences JSON</label
    ><textarea
      id="{id}-json"
      bind:value
      oninput={() => onChange(value)}
      rows="8"
      spellcheck="false"></textarea>
    <p class="hint">
      Every control above writes into this document. Edit it directly to set
      fields the form does not render.
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
  .hint {
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
  .group {
    margin-top: 1rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border-hairline);
  }
  .group-heading {
    margin: 0;
    font-size: 0.85rem;
    font-weight: 500;
  }
</style>
