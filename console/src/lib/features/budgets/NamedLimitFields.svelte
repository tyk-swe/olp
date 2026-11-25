<script lang="ts">
  import LimitFields from './LimitFields.svelte';
  import {
    namedLimitEditors,
    namedLimitRow,
    type NamedLimitKind,
    type NamedLimitRow
  } from './namedLimits';
  let {
    form = $bindable(),
    kind,
    disabled = false
  }: {
    form: NamedLimitRow[];
    kind: NamedLimitKind;
    disabled?: boolean;
  } = $props();
  const editor = $derived(namedLimitEditors[kind]);
</script>

<fieldset {disabled}>
  <legend>{editor.legend}</legend>
  <p class="muted">{editor.help}</p>
  {#each form as row (row.id)}
    <div class="limit-row">
      <div class="form-field">
        <label for={`route-limit-${row.id}`}>{editor.label}</label>
        <input
          id={`route-limit-${row.id}`}
          bind:value={row.name}
          maxlength="100"
        />
      </div>
      <LimitFields prefix={`route-limit-${row.id}`} bind:form={row.limits} />
      <button
        class="button button-secondary"
        type="button"
        onclick={() => {
          form = form.filter((entry) => entry.id !== row.id);
        }}
      >
        Remove {editor.item}
      </button>
    </div>
  {/each}
  <button
    class="button button-secondary"
    type="button"
    disabled={disabled || form.length >= editor.maxEntries}
    onclick={() => {
      form = [...form, namedLimitRow()];
    }}
  >
    Add {editor.item}
  </button>
</fieldset>

<style>
  fieldset {
    border: 0;
    padding: 0;
    margin: 1rem 0;
  }
  legend {
    font-weight: 600;
  }
  .limit-row {
    display: grid;
    gap: 1rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    padding: 1rem;
    margin: 1rem 0;
  }
</style>
