<script lang="ts">
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  const services = useServiceCapabilities();
  import RoutingPolicyEditor from '$lib/features/routes/RoutingPolicyEditor.svelte';
  import ConfigurationPanel from '$lib/features/configuration/ConfigurationPanel.svelte';
  import PricingSourcesPanel from '$lib/features/usage/PricingSourcesPanel.svelte';
  import { settingsKeys } from '$lib/features/settings/settingsKeys';
  import PricingRevisionsPanel from '$lib/features/usage/PricingRevisionsPanel.svelte';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    listSettings,
    updateSetting,
    type Setting
  } from '$lib/features/settings/api';
  import { formatDate, stateLabel } from '$lib/format';
  import {
    LIMITS_OUTAGE_POLICIES,
    RETENTION_MAX_DAYS,
    RETENTION_MIN_DAYS,
    isLimitsOutagePolicy,
    isRetentionKey
  } from '$lib/features/settings/validation';
  import {
    applyServerFieldErrors,
    errorMessage,
    isEtagMismatch
  } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';

  const queryClient = useQueryClient();
  const access = useRole();
  const canEditSettings = $derived(access.can('settings.update'));
  let savingPrice = $state(false);
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

  const settings = createQuery(() => ({
    queryKey: settingsKeys.all(),
    queryFn: () => listSettings()
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
{/if}
<PricingRevisionsPanel
  blocked={Boolean(savingKey)}
  onBusyChange={(busy) => {
    savingPrice = busy;
  }}
  onFeedback={(feedback) => {
    status = feedback.status;
    error = feedback.error;
  }}
/>
{#if services.gatewayAvailable}
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
  @media (max-width: 60rem) {
    .setting-row {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 36rem) {
    .setting-row {
      padding: 1rem;
    }
    .setting-control {
      display: grid;
    }
  }
</style>
