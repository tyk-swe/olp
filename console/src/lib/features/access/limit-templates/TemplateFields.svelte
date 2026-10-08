<script lang="ts">
  import LimitFields from '../budgets/LimitFields.svelte';
  import { limitForm } from '../budgets/limitForm';
  import type { TemplateForm } from './policy';
  let {
    form = $bindable(),
    disabled = false
  }: { form: TemplateForm; disabled?: boolean } = $props();
</script>

<fieldset {disabled}>
  <legend>Limit templates</legend>
  <p class="muted">
    Every referencing key, end-user policy and budget group inherits these
    ceilings. Inline limits may be stricter. Each boundary keeps its own
    counters.
  </p>
  {#each form as row, index (row.id)}
    <div class="route-limit">
      <div class="form-field">
        <label for={`route-limit-${row.id}`}>Template name</label>
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
          form = form.filter((_, i) => i !== index);
        }}>Remove template</button
      >
    </div>
  {/each}
  <button
    class="button button-secondary"
    type="button"
    disabled={disabled || form.length >= 64}
    onclick={() => {
      form = [
        ...form,
        { id: crypto.randomUUID(), name: '', limits: limitForm() }
      ];
    }}>Add template</button
  >
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
  .route-limit {
    display: grid;
    gap: 1rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    padding: 1rem;
    margin: 1rem 0;
  }
</style>
