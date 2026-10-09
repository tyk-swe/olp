<script lang="ts">
  import type { AttributionForm } from './policy';
  let {
    form = $bindable(),
    prefix,
    disabled = false
  }: { form: AttributionForm; prefix: string; disabled?: boolean } = $props();
</script>

<fieldset {disabled}>
  <legend>Attribution policy</legend>
  <p class="muted">
    Required labels must be supplied by the caller or a pinned default. Project
    requirements also apply. Callers cannot change pinned values.
  </p>
  <div class="form-field">
    <label for={`${prefix}-required`}>Required attribution keys</label>
    <input
      id={`${prefix}-required`}
      bind:value={form.required}
      placeholder="team, environment"
    />
    <small
      >Separate keys with commas. Caller-supplied keys must also be in the key's
      allowed attribution keys.</small
    >
  </div>
  {#each form.defaults as row (row.id)}
    <div class="pin">
      <div class="form-field">
        <label for={`${prefix}-${row.id}-key`}>Pinned label</label>
        <input
          id={`${prefix}-${row.id}-key`}
          bind:value={row.key}
          maxlength="32"
        />
      </div>
      <div class="form-field">
        <label for={`${prefix}-${row.id}-value`}>Pinned value</label>
        <input
          id={`${prefix}-${row.id}-value`}
          bind:value={row.value}
          maxlength="64"
        />
      </div>
      <button
        class="button button-secondary"
        type="button"
        onclick={() =>
          (form.defaults = form.defaults.filter(
            (entry) => entry.id !== row.id
          ))}>Remove pinned label</button
      >
    </div>
  {/each}
  <button
    class="button button-secondary"
    type="button"
    disabled={form.defaults.length >= 4}
    onclick={() =>
      form.defaults.push({ id: crypto.randomUUID(), key: '', value: '' })}
    >Add pinned label</button
  >
</fieldset>

<style>
  fieldset {
    border: 0;
    margin: 1rem 0;
    padding: 0;
    min-width: 0;
  }
  legend {
    font-weight: 600;
  }
  .pin {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
    margin: 1rem 0;
  }
  .pin .form-field {
    flex: 1 1 10rem;
  }
</style>
