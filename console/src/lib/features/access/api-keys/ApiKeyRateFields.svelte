<script lang="ts">
  import Advanced from '#lib/components/Advanced.svelte';
  import { RunLimits } from '#lib/features/budgets/index.js';
  import type { ApiKeyFormState } from './apiKeyPolicy.js';
  import type { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import { ADMISSION_PRIORITIES } from '#lib/management/enums.js';

  let {
    form,
    errors,
    canManage,
    services
  }: {
    form: ApiKeyFormState;
    errors: Record<string, string>;
    canManage: boolean;
    services: ReturnType<typeof useServiceCapabilities>;
  } = $props();
</script>

<section aria-labelledby="limits-heading">
  <h2 id="limits-heading">Rate limits</h2>
  <p class="section-help">
    {services.limitsEnforced
      ? 'Leave a field blank for no limit.'
      : 'These values are saved for future enforcement. They do not restrict requests yet. Leave blank for no limit.'}
  </p>
  <div class="form-grid limits">
    <div class="form-field">
      <label for="rpm">Requests per minute</label><input
        id="rpm"
        type="number"
        min="1"
        inputmode="numeric"
        value={form.requestsPerMinute}
        oninput={(event) =>
          (form.requestsPerMinute = event.currentTarget.value)}
        disabled={!canManage}
        aria-invalid={errors.requestsPerMinute ? 'true' : undefined}
        aria-describedby={errors.requestsPerMinute ? 'rpm-error' : undefined}
      />{#if errors.requestsPerMinute}<small class="field-error" id="rpm-error">
          {errors.requestsPerMinute}
        </small>{/if}
    </div>
    <div class="form-field">
      <label for="tpm">Tokens per minute</label><input
        id="tpm"
        type="number"
        min="1"
        inputmode="numeric"
        value={form.tokensPerMinute}
        oninput={(event) => (form.tokensPerMinute = event.currentTarget.value)}
        disabled={!canManage}
        aria-invalid={errors.tokensPerMinute ? 'true' : undefined}
        aria-describedby={errors.tokensPerMinute ? 'tpm-error' : undefined}
      />{#if errors.tokensPerMinute}<small class="field-error" id="tpm-error">
          {errors.tokensPerMinute}
        </small>{/if}
    </div>
    <div class="form-field">
      <label for="concurrency">Concurrent requests</label><input
        id="concurrency"
        type="number"
        min="1"
        inputmode="numeric"
        value={form.maxConcurrency}
        oninput={(event) => (form.maxConcurrency = event.currentTarget.value)}
        disabled={!canManage}
        aria-invalid={errors.maxConcurrency ? 'true' : undefined}
        aria-describedby={errors.maxConcurrency
          ? 'concurrency-error'
          : undefined}
      />{#if errors.maxConcurrency}<small
          class="field-error"
          id="concurrency-error"
        >
          {errors.maxConcurrency}
        </small>{/if}
    </div>
  </div>
  <Advanced
    label="Priority and agent run limits"
    open={Boolean(errors.priority)}
  >
    <div class="advanced-fields">
      <div class="form-grid limits">
        <div class="form-field">
          <label for="priority">Admission priority</label><select
            id="priority"
            bind:value={form.priority}
            disabled={!canManage}
            aria-invalid={errors.priority ? 'true' : undefined}
            aria-describedby={errors.priority
              ? 'priority-error'
              : 'priority-help'}
          >
            <option value="">Normal</option>

            {#each ADMISSION_PRIORITIES as priority (priority)}
              <option value={priority}>{priority}</option>
            {/each}
          </select>

          <small id="priority-help" class="section-help">
            The class a saturated gateway queues this key's requests in.
          </small>

          {#if errors.priority}
            <small class="field-error" id="priority-error">
              {errors.priority}
            </small>
          {/if}
        </div>
        <div class="form-field">
          <label for="max-priority">Highest requestable priority</label><select
            id="max-priority"
            bind:value={form.maxPriority}
            disabled={!canManage}
            aria-describedby="max-priority-help"
          >
            <option value="">Its default</option>

            {#each ADMISSION_PRIORITIES as priority (priority)}
              <option value={priority}>{priority}</option>
            {/each}
          </select>

          <small id="max-priority-help" class="section-help">
            Callers may ask for up to this class in the X-OLP-Routing header.
          </small>
        </div>
      </div>
      <RunLimits
        prefix="key-run"
        value={form.runLimits}
        disabled={!canManage}
        onChange={(value) => (form.runLimits = value)}
      />
    </div>
  </Advanced>
</section>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  section {
    padding-top: 1.5rem;
    border-top: 1px solid var(--border-hairline);
  }
  .advanced-fields {
    display: grid;
    gap: 1.25rem;
    padding-top: 1rem;
  }
  .limits {
    grid-template-columns: repeat(3, 1fr);
  }
  @media (max-width: 48rem) {
    .limits {
      grid-template-columns: 1fr;
    }
  }
</style>
