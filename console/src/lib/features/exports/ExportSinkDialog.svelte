<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import ProjectScopeField from '#lib/domain/projects/ProjectScopeField.svelte';
  import type { ProjectMembership } from '#lib/domain/projects/api.js';
  import { projectOptions } from '#lib/domain/projects/projectOptions.js';
  import type { ExportSink } from './api';
  import {
    changeSinkDelivery,
    toggleSinkStream,
    type SinkDelivery
  } from './sinkSettings';
  import {
    isSinkType,
    sinkOutcomeLabels,
    sinkStreamOptions,
    sinkTypeFormats,
    sinkTypeLabels,
    stripeMeterValues,
    type SinkOutcome
  } from './presentation';

  let {
    editingSink,
    busy,
    error,
    canManageInstallation,
    memberships,
    sinkName = $bindable(),
    delivery = $bindable(),
    sinkDestination = $bindable(),
    sinkProjectId = $bindable(),
    sinkFilterProject = $bindable(),
    sinkFilterRoute = $bindable(),
    sinkFilterOutcome = $bindable(),
    sinkCredential = $bindable(),
    sinkClearCredential = $bindable(),
    sinkStripeKey = $bindable(),
    sinkStripeAdvanced = $bindable(),
    onClose,
    onSubmit,
    onCancel
  }: {
    editingSink: ExportSink | null;
    busy: boolean;
    error: string;
    canManageInstallation: boolean;
    memberships: ProjectMembership[];
    sinkName: string;
    delivery: SinkDelivery;
    sinkDestination: string;
    sinkProjectId: string;
    sinkFilterProject: string;
    sinkFilterRoute: string;
    sinkFilterOutcome: '' | SinkOutcome;
    sinkCredential: string;
    sinkClearCredential: boolean;
    sinkStripeKey: string;
    sinkStripeAdvanced: boolean;
    onClose: () => void;
    onSubmit: (event: SubmitEvent) => void;
    onCancel: () => void;
  } = $props();

  const streamLock = $derived(
    delivery.format === 'cloudevents' || delivery.format === 'stripe'
      ? ['metering']
      : delivery.format === 'csv_gzip'
        ? ['focus']
        : null
  );
  const focusVisible = $derived(delivery.format === 'csv_gzip');
  const outcomeVisible = $derived(delivery.format !== 'csv_gzip');

  function selectDelivery(
    type: ExportSink['type'],
    format?: ExportSink['format']
  ) {
    delivery = changeSinkDelivery(delivery, type, format);
    if (type === 'stripe_meter' && !sinkDestination.trim())
      sinkDestination = 'https://api.stripe.com';
  }
  function toggleStream(stream: ExportSink['streams'][number]) {
    delivery = toggleSinkStream(delivery, stream);
  }
  function removeCustomer(index: number) {
    if (delivery.format !== 'stripe') return;
    delivery = {
      ...delivery,
      customers: delivery.customers.filter((_, position) => position !== index)
    };
  }
  function addCustomer() {
    if (delivery.format !== 'stripe') return;
    delivery = {
      ...delivery,
      customers: [...delivery.customers, { project: '', customer: '' }]
    };
  }
</script>

<Dialog
  title={editingSink ? 'Edit sink' : 'New telemetry sink'}
  description="Streams logs, traces and usage to a collector you run."
  placement="side"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form class="create-form" onsubmit={onSubmit}>
    <fieldset class="submission-fields" disabled={busy}>
      <div class="form-grid">
        <div class="form-field">
          <label for="sink-name">Name</label><input
            id="sink-name"
            bind:value={sinkName}
            required
          />
        </div>
        <div class="form-field">
          <label for="sink-type">Type</label><select
            id="sink-type"
            value={delivery.type}
            onchange={(event) => {
              const type = event.currentTarget.value;
              if (isSinkType(type)) selectDelivery(type);
            }}
            disabled={!!editingSink}
          >
            {#each Object.keys(sinkTypeFormats) as type (type)}<option
                value={type}
              >
                {sinkTypeLabels[type] ?? type}
              </option>{/each}
          </select>
        </div>
        <div class="form-field">
          <label for="sink-destination">Destination</label><input
            id="sink-destination"
            bind:value={sinkDestination}
            required
            placeholder="https://collector.example.com/olp or bucket URL"
          />
        </div>
        <ProjectScopeField
          id="sink-project"
          bind:value={sinkProjectId}
          unassigned={canManageInstallation}
          disabled={!!editingSink}
        />
        <div class="form-field">
          <label for="sink-format">Format</label><select
            id="sink-format"
            value={delivery.format}
            onchange={(event) => {
              const option = sinkTypeFormats[delivery.type].find(
                (option) => option.value === event.currentTarget.value
              );
              if (option) selectDelivery(delivery.type, option.value);
            }}
            disabled={!!editingSink}
          >
            {#each sinkTypeFormats[delivery.type] ?? [] as option (option.value)}<option
                value={option.value}
              >
                {option.label}
              </option>{/each}
          </select>
        </div>
        <fieldset class="form-field">
          <legend>Streams</legend>
          {#each sinkStreamOptions as stream (stream.value)}
            {#if stream.value !== 'focus' || focusVisible}
              <label class="inline-check">
                <input
                  type="checkbox"
                  checked={delivery.streams.some(
                    (value) => value === stream.value
                  )}
                  disabled={!!editingSink || streamLock !== null}
                  onchange={() => toggleStream(stream.value)}
                />{stream.label}
              </label>
            {/if}
          {/each}
          {#if streamLock}<small>
              This format carries {streamLock[0]} only.
            </small>{/if}
        </fieldset>
        <div class="form-field">
          <label for="sink-filter-project">Only for project</label><EntityPicker
            id="sink-filter-project"
            source={projectOptions}
            bind:value={sinkFilterProject}
            emptyLabel="All projects"
          />
        </div>
        <div class="form-field">
          <label for="sink-filter-route">Route filter</label><input
            id="sink-filter-route"
            bind:value={sinkFilterRoute}
            placeholder="Optional route slug"
          />
        </div>
        {#if outcomeVisible}<div class="form-field">
            <label for="sink-filter-outcome">Outcome filter</label><select
              id="sink-filter-outcome"
              bind:value={sinkFilterOutcome}
            >
              <option value="">All outcomes</option>
              {#each Object.entries(sinkOutcomeLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          </div>{/if}
        {#if delivery.type === 'stripe_meter'}
          <div class="form-field">
            <label for="sink-stripe-key">Restricted API key</label><input
              id="sink-stripe-key"
              type="password"
              autocomplete="off"
              bind:value={sinkStripeKey}
              placeholder={editingSink ? 'Blank keeps the stored key' : 'rk_…'}
            />
          </div>
        {:else}<div class="form-field">
            <label for="sink-credential">Credential JSON</label><textarea
              id="sink-credential"
              bind:value={sinkCredential}
              rows="2"
              placeholder={editingSink
                ? 'Optional sealed replacement — blank keeps current'
                : 'Optional sealed credential object'}></textarea>
            {#if editingSink}<label class="inline-check">
                <input
                  type="checkbox"
                  aria-label="Clear stored credential"
                  bind:checked={sinkClearCredential}
                />Clear stored credential
              </label>{/if}
          </div>{/if}
        {#if delivery.format === 'csv_gzip'}
          <div class="form-field">
            <label for="sink-cost-basis">FOCUS cost basis</label><select
              id="sink-cost-basis"
              bind:value={delivery.focusCostBasis}
              disabled={!!editingSink}
            >
              <option value="cost">Provider cost</option><option value="charge">
                Charge
              </option>
            </select>{#if editingSink}<small>
                Cost basis cannot change after creation.
              </small>{/if}
          </div>
          <div class="form-field">
            <label for="sink-focus-start">FOCUS start date</label><input
              id="sink-focus-start"
              type="date"
              bind:value={delivery.focusStartDate}
            /><small>
              Optional, new dates must be within the retained usage window.
            </small>
          </div>
        {:else if delivery.format === 'cloudevents'}
          <div class="form-field">
            <label for="sink-ce-source">CloudEvents source</label><input
              id="sink-ce-source"
              bind:value={delivery.ceSource}
              placeholder="https://example.com/olp"
            />
          </div>
        {:else if delivery.format === 'stripe'}
          <div class="form-field">
            <label for="sink-stripe-event">Stripe event name</label><input
              id="sink-stripe-event"
              bind:value={delivery.stripeEvent}
              required
              placeholder="olp.charge"
            />
          </div>
          <div class="form-field">
            <label for="sink-stripe-value">Meter value</label><select
              id="sink-stripe-value"
              bind:value={delivery.stripeValue}
            >
              {#each stripeMeterValues as option (option.value)}<option
                  value={option.value}
                >
                  {option.label}
                </option>{/each}
            </select>
          </div>
          <fieldset class="form-field">
            <legend>Stripe customers</legend>
            {#each delivery.customers as row, i (i)}
              <div class="customer-row">
                <select aria-label="Project" bind:value={row.project}>
                  {#each memberships as membership (membership.id)}<option
                      value={membership.id}
                    >
                      {membership.name ?? membership.id}
                    </option>{/each}
                </select><input
                  aria-label="Stripe customer"
                  class="mono"
                  bind:value={row.customer}
                  placeholder="cus_…"
                /><button
                  class="text-button"
                  type="button"
                  aria-label="Remove mapping"
                  disabled={delivery.customers.length <= 1}
                  onclick={() => removeCustomer(i)}
                >
                  &times;
                </button>
              </div>
            {/each}
            <button class="text-button" type="button" onclick={addCustomer}>
              Add mapping
            </button>
          </fieldset>
          <button
            class="text-button"
            type="button"
            aria-expanded={sinkStripeAdvanced}
            onclick={() => (sinkStripeAdvanced = !sinkStripeAdvanced)}
          >
            Advanced
          </button>
          {#if sinkStripeAdvanced}
            <div class="form-field">
              <label for="sink-stripe-customer-key">Customer key</label><input
                id="sink-stripe-customer-key"
                class="mono"
                bind:value={delivery.stripeCustomerKey}
                placeholder="olp_project"
              />
            </div>
            <div class="form-field">
              <label for="sink-stripe-value-key">Value key</label><input
                id="sink-stripe-value-key"
                class="mono"
                bind:value={delivery.stripeValueKey}
                placeholder="olp_value"
              />
            </div>
          {/if}
        {/if}
      </div>
      <div class="form-actions">
        {#if editingSink}<button
            class="button button-secondary"
            type="button"
            onclick={onCancel}
          >
            Cancel
          </button>{/if}
        <button
          class="button button-primary"
          type="submit"
          disabled={busy ||
            !sinkName.trim() ||
            !sinkDestination.trim() ||
            (!editingSink && !delivery.streams.length)}
        >
          {busy ? 'Saving…' : editingSink ? 'Save sink' : 'Create sink'}
        </button>
      </div>
    </fieldset>
  </form>
</Dialog>
