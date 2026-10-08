<script lang="ts">
  import LimitFields from '../budgets/LimitFields.svelte';
  import { limitForm } from '../budgets/limitForm';
  import type { RouteLimitForm } from './policy';
  let {
    form = $bindable(),
    disabled = false
  }: { form: RouteLimitForm; disabled?: boolean } = $props();
</script>

<fieldset {disabled}>
  <legend>Limits by route</legend>
  <p class="muted">
    Additional limits for this key on individual routes. Overall limits also
    apply. Subscription routes use rate and concurrency limits without USD
    budgets.
  </p>
  {#each form as row, index (row.id)}
    <div class="route-limit">
      <div class="form-field">
        <label for={`route-limit-${row.id}`}>Route slug</label>
        <input
          id={`route-limit-${row.id}`}
          bind:value={row.route}
          maxlength="100"
        />
      </div>
      <LimitFields prefix={`route-limit-${row.id}`} bind:form={row.limits} />
      <button
        class="button button-secondary"
        type="button"
        onclick={() => {
          form = form.filter((_, i) => i !== index);
        }}>Remove route limit</button
      >
    </div>
  {/each}
  <button
    class="button button-secondary"
    type="button"
    disabled={disabled || form.length >= 100}
    onclick={() => {
      form = [
        ...form,
        { id: crypto.randomUUID(), route: '', limits: limitForm() }
      ];
    }}>Add route limit</button
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
