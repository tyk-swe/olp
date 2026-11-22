<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import type { EntitySource } from '#lib/components/entityOptions.js';
  import type { IncreaseInput } from './api';

  let {
    kind = $bindable('api_key'),
    resourceId = $bindable(''),
    route = $bindable(''),
    digest = $bindable(''),
    label = $bindable(''),
    value = $bindable(''),
    window = $bindable('day'),
    amount = $bindable(''),
    reason = $bindable(''),
    expires = $bindable(''),
    busy,
    submitted,
    canOrganize,
    canInstall,
    targetPicker,
    kinds,
    onSubmit,
    onReset
  }: {
    kind: IncreaseInput['target']['kind'];
    resourceId: string;
    route: string;
    digest: string;
    label: string;
    value: string;
    window: IncreaseInput['window'];
    amount: string;
    reason: string;
    expires: string;
    busy: boolean;
    submitted: IncreaseInput | null;
    canOrganize: boolean;
    canInstall: boolean;
    targetPicker: { source: EntitySource; label: string } | null;
    kinds: { value: IncreaseInput['target']['kind']; label: string }[];
    onSubmit: (event: SubmitEvent) => void;
    onReset: () => void;
  } = $props();
</script>

<form onsubmit={onSubmit}>
  <fieldset disabled={busy || submitted !== null}>
    <div class="fields">
      <div class="form-field">
        <label for="increase-kind">Budget boundary</label><select
          id="increase-kind"
          bind:value={kind}
          >{#each kinds as option (option.value)}<option value={option.value}
              >{option.label}</option
            >{/each}{#if canOrganize}<option value="organization"
              >Organization</option
            >{/if}{#if canInstall}<option value="installation"
              >Installation</option
            >{/if}</select
        >
      </div>
      {#if targetPicker}<div class="form-field">
          <label for="increase-resource">{targetPicker.label}</label>
          {#key targetPicker.source}<EntityPicker
              id="increase-resource"
              source={targetPicker.source}
              bind:value={resourceId}
              emptyLabel="Choose one"
              required
            />{/key}
        </div>{:else if kind !== 'installation'}<div class="form-field">
          <label for="increase-resource">API key ID</label><input
            id="increase-resource"
            class="mono"
            bind:value={resourceId}
            required
          />
        </div>{/if}
      {#if kind === 'key_route'}<div class="form-field">
          <label for="increase-route">Route slug</label><input
            id="increase-route"
            bind:value={route}
            required
          />
        </div>{/if}
      {#if kind === 'key_end_user' || kind === 'project_end_user'}<div
          class="form-field"
        >
          <label for="increase-digest">End-user digest</label><input
            id="increase-digest"
            bind:value={digest}
            required
            pattern={'[0-9a-f]{64}'}
          /><small
            >Use the digest from end-user lookup, never a raw identifier.</small
          >
        </div>{/if}
      {#if kind === 'attribution'}<div class="form-field">
          <label for="increase-label">Attribution label</label><input
            id="increase-label"
            bind:value={label}
            required
          />
        </div>
        <div class="form-field">
          <label for="increase-value">Attribution value</label><input
            id="increase-value"
            bind:value
            required
          />
        </div>{/if}
      <div class="form-field">
        <label for="increase-window">Budget window</label><select
          id="increase-window"
          bind:value={window}
          ><option value="day">Day</option><option value="week">Week</option
          ><option value="month">Month</option></select
        >
      </div>
      <div class="form-field">
        <label for="increase-amount">Additional USD</label><input
          id="increase-amount"
          bind:value={amount}
          inputmode="decimal"
          required
        />
      </div>
      <div class="form-field">
        <label for="increase-expiry">Earlier expiry (optional)</label><input
          id="increase-expiry"
          type="datetime-local"
          bind:value={expires}
        /><small>Your local time. Blank uses the budget-window end.</small>
      </div>
      <div class="form-field">
        <label for="increase-reason">Reason</label><input
          id="increase-reason"
          bind:value={reason}
          required
          maxlength="256"
        />
      </div>
    </div>
  </fieldset>
  <button class="button button-primary" disabled={busy} type="submit"
    >{submitted ? 'Retry increase' : 'Create increase'}</button
  >
  {#if submitted}<button
      class="button button-secondary"
      type="button"
      disabled={busy}
      onclick={onReset}>Start another request</button
    >
    <p>
      Retry preserves the original request to recover a lost response without
      adding another increase.
    </p>{/if}
</form>

<style>
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 18rem), 1fr));
    gap: 1rem;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 1rem 0;
  }
</style>
