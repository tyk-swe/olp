<script lang="ts">
  import LimitFields from '../budgets/LimitFields.svelte';
  import { type PolicyForm } from './policy';
  import { limitForm } from '../budgets/limitForm';
  let {
    form = $bindable(),
    prefix,
    disabled = false
  }: { form: PolicyForm; prefix: string; disabled?: boolean } = $props();
</script>

<fieldset class="policy" {disabled}>
  <legend>Per-end-user limits</legend>
  <label class="enable"
    ><input type="checkbox" bind:checked={form.enabled} /> Enable end-user policy</label
  >
  {#if form.enabled}<div class="form-field">
      <label for={`${prefix}-limit-template`}>End-user limit template</label
      ><input
        id={`${prefix}-limit-template`}
        bind:value={form.limitTemplate}
        {disabled}
        maxlength="100"
      /><small
        >The template caps defaults and digest overrides independently. Empty
        detaches the template.</small
      >
    </div>
    <p class="muted">
      Each identified user gets these limits. Blank limits are unlimited.
      Policies on other boundaries still apply.
    </p>
    <LimitFields bind:form={form.defaults} {prefix} />
    <details>
      <summary>Digest overrides and blocking</summary>
      <p class="muted">
        An override replaces all defaults here for one digest. A blocked digest
        is refused regardless of its limits.
      </p>
      {#each form.overrides as override, index (override.id)}
        <fieldset class="override">
          <legend>Override {index + 1}</legend>
          <div class="form-field">
            <label for={`${prefix}-${override.id}-digest`}
              >End-user digest</label
            >
            <input
              id={`${prefix}-${override.id}-digest`}
              bind:value={override.digest}
              maxlength="64"
              spellcheck="false"
              autocomplete="off"
            />
          </div>
          <LimitFields
            bind:form={override.limits}
            prefix={`${prefix}-${override.id}`}
          />
          <button
            type="button"
            class="button button-secondary"
            onclick={() =>
              (form.overrides = form.overrides.filter(
                (row) => row.id !== override.id
              ))}>Remove override {index + 1}</button
          >
        </fieldset>
      {/each}
      <button
        type="button"
        class="button button-secondary"
        disabled={form.overrides.length >= 256}
        onclick={() =>
          form.overrides.push({
            id: crypto.randomUUID(),
            digest: '',
            limits: limitForm()
          })}>Add digest override</button
      >
      <div class="form-field blocked">
        <label for={`${prefix}-blocked`}>Blocked digests</label>
        <textarea
          id={`${prefix}-blocked`}
          bind:value={form.blocked}
          rows="3"
          spellcheck="false"
          aria-describedby={`${prefix}-blocked-help`}></textarea>
        <small id={`${prefix}-blocked-help`}
          >One lowercase SHA-256 digest per line, up to 256. Use the key’s
          end-user lookup to derive a digest.</small
        >
      </div>
    </details>
  {/if}
</fieldset>

<style>
  .policy {
    min-width: 0;
    margin: 1rem 0;
    padding: 0;
    border: 0;
  }
  legend {
    font-weight: 600;
    margin-bottom: 0.75rem;
  }
  .enable {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    min-height: 2.75rem;
  }
  details {
    margin-top: 1rem;
  }
  summary {
    cursor: pointer;
    min-height: 2.75rem;
    align-content: center;
  }
  .override {
    min-width: 0;
    margin: 1rem 0;
    padding: 1rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
  }
  .override > button,
  .blocked {
    margin-top: 1rem;
  }
</style>
