<script lang="ts">
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  const services = useServiceCapabilities();
  import RoutingPolicyEditor from '$lib/features/routes/RoutingPolicyEditor.svelte';
  import ConfigurationPanel from '$lib/features/configuration/ConfigurationPanel.svelte';
  import PricingSourcesPanel from '$lib/features/usage/PricingSourcesPanel.svelte';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { settingsKeys } from '$lib/features/settings/settingsKeys';
  import { pricingKeys } from '$lib/features/usage/pricingKeys';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '$lib/lists/pagination';
  import {
    createPricingRevision,
    listPricing,
    type PriceDraft
  } from '$lib/features/usage/pricing';
  import {
    listSettings,
    updateSetting,
    type Setting
  } from '$lib/features/settings/api';
  import { dateTimeLocalValue, formatDate, stateLabel } from '$lib/format';
  import { listProviderKinds } from '$lib/features/providers/models';
  import {
    listProviderVendors,
    type ProviderKind
  } from '$lib/features/providers/api';
  import {
    LIMITS_OUTAGE_POLICIES,
    RETENTION_MAX_DAYS,
    RETENTION_MIN_DAYS,
    isLimitsOutagePolicy,
    isRetentionKey,
    optionalDecimal
  } from '$lib/features/settings/validation';
  import {
    applyServerFieldErrors,
    errorMessage,
    isEtagMismatch
  } from '$lib/api/http';
  import { operationKinds } from '$lib/features/usage/history/api';
  import { useRole } from '$lib/features/access/session/useRole.svelte';

  const queryClient = useQueryClient();
  const access = useRole();
  const canEditSettings = $derived(access.can('settings.update'));
  const canEditPricing = $derived(access.can('pricing.update'));
  let values = $state<Record<string, string>>({});
  let editEtags = $state<Record<string, string>>({});
  let conflictKey = $state('');
  function setValue(setting: Setting, value: string) {
    editEtags[setting.key] ??= setting.etag;
    values[setting.key] = value;
  }
  async function reloadConflict() {
    const result = await settings.refetch();
    if (result.isSuccess) {
      delete values[conflictKey];
      delete editEtags[conflictKey];
      delete fieldErrors[conflictKey];
      conflictKey = error = '';
    }
  }
  let fieldErrors = $state<Record<string, string>>({});
  let savingKey = $state('');
  let status = $state('');
  let error = $state('');

  let providerKind = $state<ProviderKind | null>(null);
  let providerId = $state('');
  let vendorId = $state('');
  const providerVendors = createQuery(() => ({
    queryKey: ['provider-vendors'],
    enabled: services.gatewayAvailable,
    queryFn: () => listProviderVendors()
  }));
  let model = $state('');
  let operation = $state<(typeof operationKinds)[number]>('generation');
  let inputPrice = $state('');
  let cachedInputPrice = $state('');
  let cacheWritePrice = $state('');
  let cacheWrite5mPrice = $state('');
  let cacheWrite1hPrice = $state('');
  let outputPrice = $state('');
  let unitPrice = $state('');
  let currency = $state('USD');
  let effectiveAt = $state(dateTimeLocalValue(new Date()));
  let savingPrice = $state(false);
  const pricingPagination = $state(emptyCursorHistory());

  const settings = createQuery(() => ({
    queryKey: settingsKeys.all(),
    queryFn: () => listSettings()
  }));

  const providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));

  $effect(() => {
    if (!providerKind && providerKinds.data?.[0])
      providerKind = providerKinds.data[0].kind;
  });

  const pricing = createQuery(() => ({
    queryKey: pricingKeys.page(pricingPagination.cursor),
    enabled: services.limitsEnforced,
    queryFn: () => listPricing(pricingPagination.cursor)
  }));

  function settingLabel(key: string) {
    switch (key) {
      case 'auth.local_login_enabled':
        return 'Local password sign-in';
      case 'limits.valkey_unavailable':
        return 'Limit service outage policy';
      case 'retention.audit_days':
        return 'Audit retention (days)';
      case 'retention.requests_days':
        return 'Request retention (days)';
      case 'retention.usage_days':
        return 'Usage retention (days)';
    }
    return stateLabel(key).replace(/\b\w/g, (character) =>
      character.toUpperCase()
    );
  }

  const LIMITS_OUTAGE_KEY = 'limits.valkey_unavailable';

  function settingHelp(key: string) {
    if (key === 'auth.local_login_enabled')
      return 'Allow members to sign in with local passwords. Only an owner can change this; keep a usable owner sign-in method.';
    if (key === LIMITS_OUTAGE_KEY && !services.limitsEnforced)
      return 'Saved outage policy for future limit enforcement. No request limits are enforced yet.';
    if (isRetentionKey(key) && !services.retentionEnforced)
      return 'Saved retention policy. Automatic record cleanup is not enabled yet.';
    if (key === LIMITS_OUTAGE_KEY)
      return 'What rate/concurrency-only API keys get while Valkey is unreachable: fail_closed rejects them with 503; fail_open bypasses those limits and counts olp_limits_fail_open_total. Budgeted keys always fail closed. Gateways apply a change within 15 seconds.';
    if (isRetentionKey(key))
      return 'Number of days before detailed records are removed; hourly aggregates remain retained. Must be a whole number of days between 1 and 3650.';
    return 'Installation setting stored transactionally in PostgreSQL.';
  }

  async function save(setting: Setting) {
    if (
      !canEditSettings ||
      savingKey ||
      savingPrice ||
      (setting.key === 'auth.local_login_enabled' &&
        !access.can('users.manage'))
    )
      return;
    const submittedValue = values[setting.key] ?? setting.value;
    savingKey = setting.key;
    status = error = '';
    delete fieldErrors[setting.key];
    try {
      const updated = await updateSetting(
        { ...setting, etag: editEtags[setting.key] ?? setting.etag },
        submittedValue
      );
      queryClient.setQueryData<Setting[]>(settingsKeys.all(), (current) =>
        current?.map((item) => (item.key === updated.key ? updated : item))
      );
      if ((values[setting.key] ?? setting.value) === submittedValue) {
        delete values[setting.key];
        delete editEtags[setting.key];
      } else {
        editEtags[setting.key] = updated.etag;
      }
      if (setting.key === 'auth.local_login_enabled')
        await queryClient.invalidateQueries({
          queryKey: ['service-capabilities']
        });
      status = `${settingLabel(setting.key)} saved.`;
    } catch (cause) {
      if (isEtagMismatch(cause)) conflictKey = setting.key;
      const fields = applyServerFieldErrors(cause, {
        request: 'value',
        value: 'value'
      });
      if (fields.value) fieldErrors[setting.key] = fields.value;
      else error = errorMessage(cause);
    } finally {
      savingKey = '';
    }
  }

  async function addPricing(event: SubmitEvent) {
    event.preventDefault();
    if (!canEditPricing || savingPrice || savingKey) return;
    error = status = '';
    if (!providerKind || !model.trim() || !operation.trim()) {
      error = 'Provider kind, model, and operation are required.';
      return;
    }
    const submitted = {
      providerKind,
      vendorId,
      providerId,
      model,
      operation,
      inputPrice,
      cachedInputPrice,
      cacheWritePrice,
      cacheWrite5mPrice,
      cacheWrite1hPrice,
      outputPrice,
      unitPrice,
      currency,
      effectiveAt
    };
    savingPrice = true;
    try {
      const price: PriceDraft = {
        provider_kind: submitted.providerKind,
        vendor_id: submitted.vendorId || null,
        provider_id: submitted.providerId || null,
        model: submitted.model.trim(),
        operation: submitted.operation,
        input_per_million: optionalDecimal(submitted.inputPrice),
        cached_input_per_million: optionalDecimal(submitted.cachedInputPrice),
        cache_write_input_per_million: optionalDecimal(
          submitted.cacheWritePrice
        ),
        cache_write_5m_input_per_million: optionalDecimal(
          submitted.cacheWrite5mPrice
        ),
        cache_write_1h_input_per_million: optionalDecimal(
          submitted.cacheWrite1hPrice
        ),
        output_per_million: optionalDecimal(submitted.outputPrice),
        unit_price: optionalDecimal(submitted.unitPrice),
        currency: submitted.currency.trim().toUpperCase()
      };
      if (
        !price.input_per_million &&
        !price.output_per_million &&
        !price.unit_price
      )
        throw new Error('Enter at least one price.');
      await createPricingRevision(
        new Date(submitted.effectiveAt).toISOString(),
        [price]
      );
      status =
        'Pricing revision created. New usage will use the effective revision.';
      if (model === submitted.model) model = '';
      if (inputPrice === submitted.inputPrice) inputPrice = '';
      if (cachedInputPrice === submitted.cachedInputPrice)
        cachedInputPrice = '';
      if (cacheWritePrice === submitted.cacheWritePrice) cacheWritePrice = '';
      if (cacheWrite5mPrice === submitted.cacheWrite5mPrice)
        cacheWrite5mPrice = '';
      if (cacheWrite1hPrice === submitted.cacheWrite1hPrice)
        cacheWrite1hPrice = '';
      if (outputPrice === submitted.outputPrice) outputPrice = '';
      if (unitPrice === submitted.unitPrice) unitPrice = '';
      if (providerId === submitted.providerId) providerId = '';
      if (effectiveAt === submitted.effectiveAt)
        effectiveAt = dateTimeLocalValue(new Date());
      resetCursor(pricingPagination);
      await pricing.refetch();
    } catch (cause) {
      error = errorMessage(cause, 'The pricing revision could not be created.');
    } finally {
      savingPrice = false;
    }
  }
</script>

<svelte:head><title>Settings · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Installation</p>
    <h1 class="page-title">Settings</h1>
    <p class="page-description">
      {services.gatewayAvailable
        ? 'Retention, installation defaults, and versioned pricing.'
        : 'Installation access and saved policies.'} Personal details live in your
      profile.
    </p>
  </div>
  <a class="button button-secondary" href={resolve('/settings/profile')}
    >Personal profile</a
  >
</div>

{#if status}<p class="success-message" role="status">{status}</p>{/if}
{#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
{#if conflictKey}<button
    class="button button-secondary"
    onclick={reloadConflict}
    >Discard this edit and reload the current setting</button
  >{/if}

<section class="settings-section" aria-labelledby="installation-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Configuration</p>
      <h2 id="installation-title">Installation defaults</h2>
    </div>
  </div>
  {#if settings.isPending}<div class="loading-state" role="status">
      Loading settings…
    </div>
  {:else if settings.isError}<div class="inline-problem" role="alert">
      {errorMessage(settings.error)}
      <button class="text-button" onclick={() => settings.refetch()}
        >Try again</button
      >
    </div>
  {:else if settings.data?.length === 0}<div class="card empty-state">
      No editable installation settings are registered.
    </div>
  {:else}{#if !canEditSettings}<ReadOnlyNote
        >Your role can view installation settings but not change them.</ReadOnlyNote
      >{/if}
    <div class="settings-list">
      {#each settings.data ?? [] as setting (setting.key)}{@const fieldProblem =
          fieldErrors[setting.key]}
        <article class="card setting-row">
          <div>
            <label for={`setting-${setting.key}`}
              >{settingLabel(setting.key)}</label
            >
            <p>{settingHelp(setting.key)}</p>
            <small
              >Updated {formatDate(setting.updated_at)} by
              <span class="mono">{setting.updated_by}</span></small
            >
          </div>
          <div class="setting-control">
            {#if setting.key === 'auth.local_login_enabled'}<select
                id={`setting-${setting.key}`}
                value={values[setting.key] ?? setting.value}
                onchange={(event) =>
                  setValue(setting, event.currentTarget.value)}
                disabled={!access.can('users.manage')}
              >
                <option value="true">Enabled</option><option value="false"
                  >Disabled</option
                >
              </select>
            {:else if setting.key === LIMITS_OUTAGE_KEY}<select
                id={`setting-${setting.key}`}
                value={isLimitsOutagePolicy(
                  values[setting.key] ?? setting.value
                )
                  ? (values[setting.key] ?? setting.value)
                  : 'fail_closed'}
                onchange={(event) =>
                  setValue(setting, event.currentTarget.value)}
                disabled={!canEditSettings}
                aria-invalid={fieldProblem ? 'true' : undefined}
                aria-describedby={fieldProblem
                  ? `setting-${setting.key}-error`
                  : undefined}
                >{#each LIMITS_OUTAGE_POLICIES as policy (policy)}<option
                    value={policy}>{policy}</option
                  >{/each}</select
              >{:else if isRetentionKey(setting.key)}<input
                id={`setting-${setting.key}`}
                type="number"
                inputmode="numeric"
                min={RETENTION_MIN_DAYS}
                max={RETENTION_MAX_DAYS}
                step="1"
                value={values[setting.key] ?? setting.value}
                oninput={(event) =>
                  setValue(setting, event.currentTarget.value)}
                readonly={!canEditSettings}
                aria-invalid={fieldProblem ? 'true' : undefined}
                aria-describedby={fieldProblem
                  ? `setting-${setting.key}-error`
                  : undefined}
              />{:else}<input
                id={`setting-${setting.key}`}
                value={values[setting.key] ?? setting.value}
                oninput={(event) =>
                  setValue(setting, event.currentTarget.value)}
                readonly={!canEditSettings}
                aria-invalid={fieldProblem ? 'true' : undefined}
                aria-describedby={fieldProblem
                  ? `setting-${setting.key}-error`
                  : undefined}
              />{/if}<button
              class="button button-secondary"
              type="button"
              onclick={() => save(setting)}
              disabled={!canEditSettings ||
                (setting.key === 'auth.local_login_enabled' &&
                  !access.can('users.manage')) ||
                Boolean(savingKey) ||
                savingPrice ||
                (values[setting.key] ?? setting.value) === setting.value}
              >{savingKey === setting.key ? 'Saving…' : 'Save'}</button
            >{#if fieldProblem}<small
                class="field-error"
                id={`setting-${setting.key}-error`}
                role="alert">{fieldProblem}</small
              >{/if}
          </div>
        </article>{/each}
    </div>{/if}
</section>

{#if services.gatewayAvailable}
  <PricingSourcesPanel />
  <section class="settings-section" aria-labelledby="pricing-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">Cost estimates</p>
        <h2 id="pricing-title">Pricing revisions</h2>
        <p>
          Prices are exact decimals. A missing price remains visibly unpriced
          and is never treated as zero. An empty cached input rate bills cached
          tokens at the full input rate.
        </p>
      </div>
    </div>
    {#if !canEditPricing}<ReadOnlyNote
        >Your role can view pricing revisions but not create them.</ReadOnlyNote
      >{/if}
    <form class="card price-form" onsubmit={addPricing}>
      <div class="form-grid">
        <div class="form-field">
          <label for="provider-kind">Provider kind</label><select
            id="provider-kind"
            bind:value={providerKind}
            onchange={() => {
              vendorId = '';
            }}
            disabled={!canEditPricing ||
              providerKinds.isPending ||
              providerKinds.isError}
            >{#each providerKinds.data ?? [] as option (option.kind)}<option
                value={option.kind}>{option.label}</option
              >{/each}</select
          >{#if providerKinds.isPending}<small
              >Loading provider capabilities…</small
            >{:else if providerKinds.isError}<small class="inline-problem"
              >Provider capabilities are unavailable; pricing changes are
              disabled.</small
            >{/if}
        </div>
        <div class="form-field">
          <label for="price-vendor">Vendor scope</label><select
            id="price-vendor"
            bind:value={vendorId}
            disabled={!canEditPricing}
            ><option value="">All vendors using this connector</option
            >{#each (providerVendors.data ?? []).filter((vendor) => vendor.connector === providerKind) as vendor (vendor.id)}<option
                value={vendor.id}>{vendor.name}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="provider-id">Provider ID override</label><input
            id="provider-id"
            bind:value={providerId}
            class="mono"
            placeholder="Optional UUID"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="price-model">Upstream model</label><input
            id="price-model"
            bind:value={model}
            required
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="price-operation">Operation</label><select
            id="price-operation"
            bind:value={operation}
            disabled={!canEditPricing}
            >{#each operationKinds as option (option)}<option value={option}
                >{option}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="input-price">Input / million</label><input
            id="input-price"
            bind:value={inputPrice}
            inputmode="decimal"
            placeholder="2.50"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="cached-input-price">Cached input / million</label><input
            id="cached-input-price"
            bind:value={cachedInputPrice}
            inputmode="decimal"
            placeholder="0.25"
            disabled={!canEditPricing}
          /><small
            >Leave empty to bill cached tokens at the full input rate.</small
          >
        </div>
        <div class="form-field">
          <label for="cache-write-price">Cache write / million</label><input
            id="cache-write-price"
            bind:value={cacheWritePrice}
            inputmode="decimal"
            placeholder="3.00"
            disabled={!canEditPricing}
          /><small
            >Leave empty to bill cache writes at the full input rate.</small
          >
        </div>
        <div class="form-field">
          <label for="cache-write-5m-price">Cache write 5m / million</label
          ><input
            id="cache-write-5m-price"
            bind:value={cacheWrite5mPrice}
            inputmode="decimal"
            placeholder="3.00"
            disabled={!canEditPricing}
          /><small>Defaults to the cache write rate, then input.</small>
        </div>
        <div class="form-field">
          <label for="cache-write-1h-price">Cache write 1h / million</label
          ><input
            id="cache-write-1h-price"
            bind:value={cacheWrite1hPrice}
            inputmode="decimal"
            placeholder="6.00"
            disabled={!canEditPricing}
          /><small>Defaults to the cache write rate, then input.</small>
        </div>
        <div class="form-field">
          <label for="output-price">Output / million</label><input
            id="output-price"
            bind:value={outputPrice}
            inputmode="decimal"
            placeholder="10.00"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="unit-price">Media unit price</label><input
            id="unit-price"
            bind:value={unitPrice}
            inputmode="decimal"
            placeholder="0.04"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="currency">Currency</label><input
            id="currency"
            bind:value={currency}
            maxlength="3"
            required
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field full">
          <label for="effective-at">Effective at</label><input
            id="effective-at"
            bind:value={effectiveAt}
            type="datetime-local"
            required
            disabled={!canEditPricing}
          />
        </div>
      </div>
      <button
        class="button button-primary"
        type="submit"
        disabled={!canEditPricing ||
          !services.limitsEnforced ||
          Boolean(savingKey) ||
          savingPrice ||
          !providerKind ||
          providerKinds.isError}
        >{savingPrice ? 'Creating…' : 'Create pricing revision'}</button
      >
    </form>

    {#if services.pending}<div class="loading-state" role="status">
        Loading installation capabilities…
      </div>
    {:else if services.error}<div class="inline-problem" role="alert">
        Installation capabilities are unavailable, so pricing revisions cannot
        be listed.
        <button
          class="text-button"
          type="button"
          onclick={() => services.retry()}>Try again</button
        >
      </div>
    {:else if !services.limitsEnforced}<div
        class="card empty-state"
        role="status"
      >
        Pricing revisions become available once limits and accounting are
        enforced by this installation.
      </div>
    {:else if pricing.isPending}<div class="loading-state" role="status">
        Loading revisions…
      </div>
    {:else if pricing.isError}<div class="inline-problem" role="alert">
        {errorMessage(pricing.error)}
        <button class="text-button" onclick={() => pricing.refetch()}
          >Try again</button
        >
      </div>
    {:else if pricing.data?.items.length === 0 && pricingPagination.history.length === 0}<div
        class="card empty-state"
      >
        No pricing revisions. Usage cost will be marked unpriced.
      </div>
    {:else}
      <div class="revision-list">
        {#each pricing.data?.items ?? [] as revision (revision.id)}
          <details class="card">
            <summary
              ><span
                ><strong>Revision {revision.revision}</strong><small
                  >Effective {formatDate(revision.effective_at)}</small
                ><small
                  >Created {formatDate(revision.created_at)} by
                  <span class="mono">{revision.created_by}</span></small
                >{#if revision.source_name}<small
                    >Published from
                    <span class="mono">{revision.source_name}</span></small
                  >{/if}</span
              ><span class="badge">{revision.prices.length} entries</span
              ></summary
            >
            <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
            <div
              class="table-shell"
              tabindex="0"
              role="region"
              aria-label={`Prices in revision ${revision.revision}`}
            >
              <table class="data-table">
                <caption class="sr-only"
                  >Prices in revision {revision.revision}</caption
                >
                <thead
                  ><tr
                    ><th scope="col">Provider / model</th><th scope="col"
                      >Operation</th
                    ><th scope="col">Input / million</th><th scope="col"
                      >Cached input / million</th
                    ><th scope="col">Cache write / million</th><th scope="col"
                      >Write 5m / million</th
                    ><th scope="col">Write 1h / million</th><th scope="col"
                      >Output / million</th
                    ><th scope="col">Unit</th><th scope="col">Currency</th></tr
                  ></thead
                >
                <tbody
                  >{#each revision.prices as price, priceIndex (`${price.provider_kind}:${price.model}:${price.operation}:${priceIndex}`)}<tr
                      ><td
                        ><strong
                          >{price.vendor_id ?? price.provider_kind}</strong
                        >{#if price.provider_id}<small
                            >{price.provider_id}</small
                          >{/if}<small>{price.model}</small></td
                      ><td>{price.operation}</td><td
                        >{price.input_per_million ?? '—'}</td
                      ><td
                        >{price.cached_input_per_million ??
                          'Billed as input'}</td
                      ><td
                        >{price.cache_write_input_per_million ??
                          'Billed as input'}</td
                      ><td>{price.cache_write_5m_input_per_million ?? '—'}</td
                      ><td>{price.cache_write_1h_input_per_million ?? '—'}</td
                      ><td>{price.output_per_million ?? '—'}</td><td
                        >{price.unit_price ?? '—'}</td
                      ><td>{price.currency}</td></tr
                    >{/each}</tbody
                >
              </table>
            </div>
          </details>
        {/each}
      </div>
      <CursorPagination
        {...cursorPaginationProps(pricingPagination, pricing.data?.nextCursor)}
        label="Pricing revision pages"
      />
    {/if}
  </section>

  <RoutingPolicyEditor
    scope="installation"
    id="00000000-0000-0000-0000-000000000000"
    canManage={canEditSettings}
  />
{/if}

{#if access.globalScope && access.can('providers.manage')}
  <ConfigurationPanel />
{/if}

<style>
  .settings-section {
    margin-top: 2.5rem;
  }
  .section-heading {
    margin-bottom: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .section-heading p:last-child {
    max-width: 44rem;
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    line-height: 1.5;
  }
  .settings-list {
    display: grid;
    gap: 0.75rem;
  }
  .setting-row {
    display: grid;
    grid-template-columns: minmax(18rem, 1fr) minmax(20rem, 0.8fr);
    gap: 1rem;
    align-items: center;
    padding: 1.25rem;
  }
  .setting-row label {
    font-weight: 500;
  }
  .setting-row p {
    margin: 0.25rem 0 0;
    color: var(--foreground-muted);
    line-height: 1.5;
  }
  .setting-row small {
    display: block;
    margin: 0.25rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .setting-control {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  .setting-control .field-error {
    flex-basis: 100%;
    color: var(--danger);
    font-size: var(--text-caption);
  }
  .setting-control input,
  .setting-control select {
    min-width: 0;
    flex: 1;
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
  }
  .setting-control input:hover,
  .setting-control select:hover {
    border-color: var(--border-strong);
  }
  .price-form {
    padding: 1.5rem;
  }
  .price-form .button {
    margin-top: 1rem;
  }
  .form-field small {
    display: block;
    margin-top: 0.25rem;
    font-size: var(--text-caption);
  }
  summary .mono {
    font-size: var(--text-caption);
    overflow-wrap: anywhere;
  }
  .revision-list {
    display: grid;
    gap: 0.75rem;
    margin-top: 1rem;
  }
  details {
    overflow: hidden;
  }
  /* The price table sits flush inside its revision card; the hairline on top
     separates it from the summary instead of stacking a second frame. */
  details .table-shell {
    border: 0;
    border-top: 1px solid var(--border-hairline);
    border-radius: 0;
  }
  summary {
    display: flex;
    min-height: 3.5rem;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.75rem 1.25rem;
    cursor: pointer;
  }
  summary strong,
  summary small,
  td strong,
  td small {
    display: block;
  }
  summary small,
  td small {
    color: var(--foreground-muted);
  }
  @media (max-width: 60rem) {
    .setting-row {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 36rem) {
    .setting-row,
    .price-form {
      padding: 1rem;
    }
    .setting-control {
      display: grid;
    }
  }
</style>
