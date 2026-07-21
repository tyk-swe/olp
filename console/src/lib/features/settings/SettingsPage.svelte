<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    createPricingRevision,
    listPricing,
    listSettings,
    updateSetting,
    type PriceDraft,
    type Setting
  } from '$lib/api/operations';
  import { dateTimeLocalValue, formatDate } from '$lib/features/operations/format';
  import { connectorOptions } from '$lib/features/gateway/providerOptions';
  import { optionalDecimal } from './validation';

  let values = $state<Record<string, string>>({});
  let savingKey = $state('');
  let status = $state('');
  let error = $state('');

  const providerKindOptions = connectorOptions.map(([kind]) => kind);
  const operationOptions = [
    'generation', 'embeddings', 'token_count', 'image_generation', 'image_edit',
    'image_variation', 'speech', 'transcription', 'video_create', 'video_list',
    'video_get', 'video_content', 'video_delete', 'moderation', 'model_list', 'model_get'
  ] as const;
  let providerKind = $state<(typeof providerKindOptions)[number]>('openai');
  let providerId = $state('');
  let model = $state('');
  let operation = $state<(typeof operationOptions)[number]>('generation');
  let inputPrice = $state('');
  let outputPrice = $state('');
  let unitPrice = $state('');
  let currency = $state('USD');
  let effectiveAt = $state(dateTimeLocalValue(new Date()));
  let savingPrice = $state(false);
  let pricingCursor = $state<string | undefined>();
  let pricingHistory = $state<Array<string | undefined>>([]);

  const settings = createQuery(() => ({
    queryKey: ['settings'],
    queryFn: async () => {
      const data = await listSettings();
      values = Object.fromEntries(data.map((setting) => [setting.key, setting.value]));
      return data;
    }
  }));

  const pricing = createQuery(() => ({
    queryKey: ['pricing', pricingCursor ?? 'first'],
    queryFn: () => listPricing(pricingCursor)
  }));

  function nextPricingPage() {
    const next = pricing.data?.next_cursor ?? undefined;
    if (!next) return;
    pricingHistory = [...pricingHistory, pricingCursor];
    pricingCursor = next;
  }

  function previousPricingPage() {
    pricingCursor = pricingHistory.at(-1);
    pricingHistory = pricingHistory.slice(0, -1);
  }

  function settingLabel(key: string) {
    return key.replaceAll('_', ' ').replace(/\b\w/g, (character) => character.toUpperCase());
  }

  function settingHelp(key: string) {
    if (key.includes('retention')) return 'Number of days before detailed records are removed; hourly aggregates remain retained.';
    if (key.includes('origin')) return 'Exact browser origin allowed for session mutations.';
    if (key.includes('oidc')) return 'Installation-level OIDC behavior; identity linking remains explicit.';
    return 'Installation setting stored transactionally in PostgreSQL.';
  }

  async function save(setting: Setting) {
    savingKey = setting.key;
    status = error = '';
    try {
      await updateSetting(setting, values[setting.key] ?? '');
      status = `${settingLabel(setting.key)} saved.`;
      await settings.refetch();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'The setting could not be saved.';
    } finally {
      savingKey = '';
    }
  }

  async function addPricing(event: SubmitEvent) {
    event.preventDefault();
    error = status = '';
    if (!model.trim() || !operation.trim()) {
      error = 'Model and operation are required.';
      return;
    }
    savingPrice = true;
    try {
      const price: PriceDraft = {
        provider_kind: providerKind,
        provider_id: providerId || null,
        model: model.trim(),
        operation,
        input_per_million: optionalDecimal(inputPrice),
        output_per_million: optionalDecimal(outputPrice),
        unit_price: optionalDecimal(unitPrice),
        currency: currency.trim().toUpperCase()
      };
      if (!price.input_per_million && !price.output_per_million && !price.unit_price) throw new Error('Enter at least one price.');
      await createPricingRevision(new Date(effectiveAt).toISOString(), [price]);
      status = 'Pricing revision created. New usage will use the effective revision.';
      model = inputPrice = outputPrice = unitPrice = providerId = '';
      effectiveAt = dateTimeLocalValue(new Date());
      pricingCursor = undefined;
      pricingHistory = [];
      await pricing.refetch();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'The pricing revision could not be created.';
    } finally {
      savingPrice = false;
    }
  }
</script>

<svelte:head><title>Settings · OpenLLMProxy</title></svelte:head>

<div class="page-header"><div><p class="eyebrow">Installation</p><h1 class="page-title">Settings</h1><p class="page-description">Retention, installation defaults, and versioned pricing. Personal details live in your profile.</p></div><a class="button button-secondary" href={resolve('/settings/profile')}>Personal profile</a></div>

{#if status}<p class="success-message" role="status">{status}</p>{/if}
{#if error}<p class="inline-problem" role="alert">{error}</p>{/if}

<section class="settings-section" aria-labelledby="installation-title"><div class="section-heading"><div><p class="eyebrow">Configuration</p><h2 id="installation-title">Installation defaults</h2></div></div>
  {#if settings.isPending}<div class="loading-state" role="status">Loading settings…</div>
  {:else if settings.isError}<div class="inline-problem" role="alert">Settings are unavailable. <button class="text-button" onclick={() => settings.refetch()}>Try again</button></div>
  {:else if settings.data?.length === 0}<div class="card empty-state">No editable installation settings are registered.</div>
  {:else}<div class="settings-list">{#each settings.data ?? [] as setting (setting.key)}<article class="card setting-row"><div><label for={`setting-${setting.key}`}>{settingLabel(setting.key)}</label><p>{settingHelp(setting.key)}</p><small>Updated {formatDate(setting.updated_at)}</small></div><div class="setting-control"><input id={`setting-${setting.key}`} value={values[setting.key] ?? setting.value} oninput={(event) => (values[setting.key] = event.currentTarget.value)} /><button class="button button-secondary" type="button" onclick={() => save(setting)} disabled={savingKey === setting.key}>{savingKey === setting.key ? 'Saving…' : 'Save'}</button></div></article>{/each}</div>{/if}
</section>

<section class="settings-section" aria-labelledby="pricing-title"><div class="section-heading"><div><p class="eyebrow">Cost estimates</p><h2 id="pricing-title">Pricing revisions</h2><p>Prices are exact decimals. A missing price remains visibly unpriced and is never treated as zero.</p></div></div>
  <form class="card price-form" onsubmit={addPricing}>
    <div class="form-grid">
      <div class="form-field"><label for="provider-kind">Provider kind</label><select id="provider-kind" bind:value={providerKind}>{#each providerKindOptions as kind (kind)}<option value={kind}>{kind}</option>{/each}</select></div>
      <div class="form-field"><label for="provider-id">Provider ID override</label><input id="provider-id" bind:value={providerId} class="mono" placeholder="Optional UUID" /></div>
      <div class="form-field"><label for="price-model">Upstream model</label><input id="price-model" bind:value={model} required /></div>
      <div class="form-field"><label for="price-operation">Operation</label><select id="price-operation" bind:value={operation}>{#each operationOptions as option (option)}<option value={option}>{option}</option>{/each}</select></div>
      <div class="form-field"><label for="input-price">Input / million</label><input id="input-price" bind:value={inputPrice} inputmode="decimal" placeholder="2.50" /></div>
      <div class="form-field"><label for="output-price">Output / million</label><input id="output-price" bind:value={outputPrice} inputmode="decimal" placeholder="10.00" /></div>
      <div class="form-field"><label for="unit-price">Media unit price</label><input id="unit-price" bind:value={unitPrice} inputmode="decimal" placeholder="0.04" /></div>
      <div class="form-field"><label for="currency">Currency</label><input id="currency" bind:value={currency} maxlength="3" required /></div>
      <div class="form-field full"><label for="effective-at">Effective at</label><input id="effective-at" bind:value={effectiveAt} type="datetime-local" required /></div>
    </div>
    <button class="button button-primary" type="submit" disabled={savingPrice}>{savingPrice ? 'Creating…' : 'Create pricing revision'}</button>
  </form>

  {#if pricing.isPending}<div class="loading-state" role="status">Loading revisions…</div>
  {:else if pricing.data?.data.length === 0 && pricingHistory.length === 0}<div class="card empty-state">No pricing revisions. Usage cost will be marked unpriced.</div>
  {:else}
    <div class="revision-list">
      {#each pricing.data?.data ?? [] as revision (revision.id)}
        <details class="card">
          <summary><span><strong>Revision {revision.revision}</strong><small>Effective {formatDate(revision.effective_at)}</small></span><span class="badge">{revision.prices.length} entries</span></summary>
          <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
          <div class="table-shell" tabindex="0" role="region" aria-label={`Prices in revision ${revision.revision}`}>
            <table class="data-table">
              <caption class="sr-only">Prices in revision {revision.revision}</caption>
              <thead><tr><th>Provider / model</th><th>Operation</th><th>Input / million</th><th>Output / million</th><th>Unit</th><th>Currency</th></tr></thead>
              <tbody>{#each revision.prices as price, priceIndex (`${price.provider_kind}:${price.model}:${price.operation}:${priceIndex}`)}<tr><td><strong>{price.provider_kind}</strong><small>{price.model}</small></td><td>{price.operation}</td><td>{price.input_per_million ?? '—'}</td><td>{price.output_per_million ?? '—'}</td><td>{price.unit_price ?? '—'}</td><td>{price.currency}</td></tr>{/each}</tbody>
            </table>
          </div>
        </details>
      {/each}
    </div>
    <CursorPagination page={pricingHistory.length + 1} hasPrevious={pricingHistory.length > 0} hasNext={Boolean(pricing.data?.next_cursor)} onPrevious={previousPricingPage} onNext={nextPricingPage} label="Pricing revision pages" />
  {/if}
</section>

<style>
  .settings-section { margin-top: 2rem; }
  .section-heading { margin-bottom: 0.8rem; }
  h2 { margin: 0; font-size: 1.2rem; }
  .section-heading p:last-child { margin: 0.3rem 0 0; color: var(--foreground-muted); }
  .success-message { margin: 1rem 0 0; padding: 0.8rem 1rem; border-radius: 0.375rem; background: var(--success-soft); color: var(--success); font-weight: 700; }
  .settings-list { display: grid; gap: 0.7rem; }
  .setting-row { display: grid; grid-template-columns: minmax(18rem, 1fr) minmax(20rem, 0.8fr); gap: 1rem; align-items: center; padding: 1rem; }
  .setting-row label { font-weight: 750; }
  .setting-row p, .setting-row small { display: block; margin: 0.2rem 0 0; color: var(--foreground-muted); font-size: 0.75rem; }
  .setting-control { display: flex; gap: 0.6rem; }
  .setting-control input { min-width: 0; flex: 1; min-height: 2.5rem; padding: 0.5rem 0.7rem; border: 1px solid var(--border-strong); border-radius: 0.375rem; background: var(--surface); color: var(--foreground); }
  .price-form { padding: 1.25rem; }
  .price-form .button { margin-top: 1rem; }
  .form-field input, .form-field select { width: 100%; }
  .revision-list { display: grid; gap: 0.7rem; margin-top: 1rem; }
  details { overflow: hidden; }
  summary { display: flex; min-height: 3.5rem; align-items: center; justify-content: space-between; gap: 1rem; padding: 0.8rem 1rem; cursor: pointer; }
  summary strong, summary small, td strong, td small { display: block; }
  summary small, td small { color: var(--foreground-muted); }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  @media (max-width: 60rem) { .setting-row { grid-template-columns: 1fr; } }
  @media (max-width: 36rem) { .setting-row, .price-form { padding: 0.85rem; } .setting-control { display: grid; } }
</style>
