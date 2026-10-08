<script lang="ts">
  import { limitFields, type LimitForm } from './limitForm';
  let { form = $bindable(), prefix }: { form: LimitForm; prefix: string } =
    $props();
</script>

<div class="limits-grid">
  {#each limitFields as field (field.key)}
    <div class="form-field">
      <label for={`${prefix}-${field.key}`}>{field.label}</label>
      <input
        id={`${prefix}-${field.key}`}
        type="text"
        inputmode={field.numeric ? 'numeric' : 'decimal'}
        bind:value={form[field.key]}
        placeholder="Unlimited"
      />
    </div>
  {/each}
</div>

<style>
  .limits-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
    gap: 1rem;
  }
</style>
