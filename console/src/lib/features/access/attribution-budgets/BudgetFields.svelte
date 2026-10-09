<script lang="ts">
  import { limitForm } from '../budgets/limitForm';
  import { costFields, type BudgetForm } from './policy';
  let {
    form = $bindable(),
    disabled = false
  }: { form: BudgetForm; disabled?: boolean } = $props();
</script>

<fieldset {disabled}>
  <legend>Attribution budgets</legend>
  <p class="muted">
    Cap project spend by label and value across keys. Caller-selected labels are
    allocation controls. Require or pin labels in attribution policy to prevent
    callers from omitting or changing them.
  </p>
  {#each form as row (row.id)}
    <div class="budget-pair">
      <div class="form-field">
        <label for={`budget-label-${row.id}`}>Attribution label</label><input
          id={`budget-label-${row.id}`}
          bind:value={row.label}
          maxlength="32"
        />
      </div>
      <div class="form-field">
        <label for={`budget-value-${row.id}`}>Attribution value</label><input
          id={`budget-value-${row.id}`}
          bind:value={row.value}
          maxlength="64"
        />
      </div>
      {#each costFields as field (field.key)}
        <div class="form-field">
          <label for={`${row.id}-${field.key}`}>{field.label}</label><input
            id={`${row.id}-${field.key}`}
            bind:value={row.limits[field.key]}
            inputmode="decimal"
            placeholder="Unlimited"
          />
        </div>
      {/each}
      <button
        type="button"
        class="button button-secondary"
        onclick={() => {
          form = form.filter((item) => item.id !== row.id);
        }}>Remove attribution budget</button
      >
    </div>
  {/each}
  <button
    type="button"
    class="button button-secondary"
    disabled={disabled || form.length >= 64}
    onclick={() => {
      form = [
        ...form,
        { id: crypto.randomUUID(), label: '', value: '', limits: limitForm() }
      ];
    }}>Add attribution budget</button
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
  .budget-pair {
    display: grid;
    gap: 1rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    padding: 1rem;
    margin: 1rem 0;
  }
</style>
