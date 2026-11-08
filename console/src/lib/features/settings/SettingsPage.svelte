<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import BrandingPanel from './BrandingPanel.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import { sessionKeys } from '#lib/features/access/session/sessionKeys.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import {
    listSettings,
    updateSetting,
    type Setting
  } from '#lib/features/settings/api.js';
  import { settingsKeys } from '#lib/features/settings/settingsKeys.js';
  import {
    LIMITS_OUTAGE_POLICIES,
    RETENTION_MAX_DAYS,
    RETENTION_MIN_DAYS,
    isLimitsOutagePolicy,
    isRetentionKey
  } from '#lib/features/settings/validation.js';
  import { formatDate, stateLabel } from '#lib/format.js';
  import {
    applyServerFieldErrors,
    errorMessage,
    isEtagMismatch
  } from '#lib/management/http.js';

  type View = 'general' | 'security' | 'retention';
  let { view }: { view: View } = $props();

  const LIMITS_OUTAGE_KEY = 'limits.valkey_unavailable';
  const SIGN_IN_KEYS = ['auth.local_login_enabled', 'auth.mfa_required'];
  const OUTAGE_LABELS: Record<(typeof LIMITS_OUTAGE_POLICIES)[number], string> =
    {
      fail_closed: 'Reject the request',
      fail_open: 'Let the request through without limits'
    };

  const queryClient = useQueryClient();
  const access = useRole();
  const services = useServiceCapabilities();
  const settings = createQuery(() => ({
    queryKey: settingsKeys.all(),
    queryFn: () => listSettings()
  }));

  let drafts = $state<Record<string, { value: string; etag: string }>>({});
  let fieldErrors = $state<Record<string, string>>({});
  let savingKey = $state('');
  let conflictKey = $state('');
  let problem = $state('');

  const viewOf = (key: string): View =>
    SIGN_IN_KEYS.includes(key)
      ? 'security'
      : isRetentionKey(key)
        ? 'retention'
        : 'general';
  const shown = $derived(
    (settings.data ?? []).filter((setting) => viewOf(setting.key) === view)
  );
  const editable = (setting: Setting) =>
    access.can('settings.update') &&
    (!SIGN_IN_KEYS.includes(setting.key) || access.can('users.manage'));
  const valueOf = (setting: Setting) =>
    drafts[setting.key]?.value ?? setting.value;

  function label(key: string) {
    switch (key) {
      case 'auth.local_login_enabled':
        return 'Email and password sign-in';
      case 'auth.mfa_required':
        return 'Require two-factor sign-in';
      case 'budgets.time_zone':
        return 'Budget time zone';
      case LIMITS_OUTAGE_KEY:
        return 'When rate limits can’t be checked';
      case 'retention.audit_days':
        return 'Keep audit log for';
      case 'retention.requests_days':
        return 'Keep request details for';
      case 'retention.usage_days':
        return 'Keep usage details for';
    }
    return stateLabel(key).replace(/^\w/, (character) =>
      character.toUpperCase()
    );
  }

  function help(key: string) {
    switch (key) {
      case 'auth.local_login_enabled':
        return 'Members can sign in with a password set in this console. Keep at least one owner able to sign in before turning this off. Only owners can change it.';
      case 'auth.mfa_required':
        return 'Password sign-ins must also use an authenticator app or security key. Single sign-on follows your identity provider instead. Only owners can change it.';
      case 'budgets.time_zone':
        return 'Daily, weekly and monthly budgets reset at midnight in this zone, for example America/New_York. Changing it never resets current spend.';
      case LIMITS_OUTAGE_KEY:
        return services.limitsEnforced
          ? 'Applies to keys with rate or concurrency limits only. Keys with a budget are always rejected. Gateways pick up a change within 15 seconds.'
          : 'Saved for later. Request limits aren’t enforced on this installation yet.';
    }
    if (isRetentionKey(key))
      return services.retentionEnforced
        ? `Older records are deleted. Hourly totals are always kept. Between ${RETENTION_MIN_DAYS} and ${RETENTION_MAX_DAYS} days.`
        : 'Saved for later. Automatic cleanup isn’t enabled on this installation yet.';
    return '';
  }

  function edit(setting: Setting, value: string) {
    drafts[setting.key] = {
      value,
      etag: drafts[setting.key]?.etag ?? setting.etag
    };
  }

  function discard(key: string) {
    delete drafts[key];
    delete fieldErrors[key];
  }

  async function reloadConflict() {
    const result = await settings.refetch();
    if (!result.isSuccess) return;
    discard(conflictKey);
    conflictKey = problem = '';
  }

  async function save(event: SubmitEvent, setting: Setting) {
    event.preventDefault();
    const draft = drafts[setting.key];
    if (!draft || !editable(setting) || savingKey) return;
    savingKey = setting.key;
    problem = '';
    delete fieldErrors[setting.key];
    try {
      const updated = await updateSetting(
        { ...setting, etag: draft.etag },
        draft.value
      );
      queryClient.setQueryData<Setting[]>(settingsKeys.all(), (current) =>
        current?.map((item) => (item.key === updated.key ? updated : item))
      );
      if (drafts[setting.key]?.value === draft.value)
        delete drafts[setting.key];
      if (setting.key === 'auth.local_login_enabled')
        await queryClient.invalidateQueries({
          queryKey: sessionKeys.serviceCapabilities
        });
      notify(`${label(setting.key)} saved`);
    } catch (cause) {
      if (isEtagMismatch(cause)) conflictKey = setting.key;
      const fields = applyServerFieldErrors(cause, {
        request: 'value',
        value: 'value'
      });
      if (fields.value) fieldErrors[setting.key] = fields.value;
      else problem = errorMessage(cause);
    } finally {
      savingKey = '';
    }
  }
</script>

<svelte:head><title>Settings · OpenLLMProxy</title></svelte:head>

{#if view === 'general'}<BrandingPanel />{/if}

{#if view === 'security' && services.managementNetworkRestricted}
  <Callout tone="info"
    >This console only accepts connections from networks set in the deployment
    configuration. Change that list in the deployment, not here.</Callout
  >
{/if}

{#if conflictKey}
  <Callout tone="warning" title="Someone else changed this setting">
    Reload to see their value, then make your change again.
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={reloadConflict}>Reload</button
      >{/snippet}
  </Callout>
{/if}
{#if problem}<Callout tone="danger">{problem}</Callout>{/if}

{#if settings.isPending}
  <div class="loading-state" role="status">Loading settings…</div>
{:else if settings.isError}
  <Callout tone="danger" title="Settings didn’t load">
    {errorMessage(settings.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => settings.refetch()}>Try again</button
      >{/snippet}
  </Callout>
{:else if shown.length === 0}
  <EmptyState title="No settings here yet" />
{:else}
  <div class="settings card">
    {#each shown as setting (setting.key)}
      {@const id = `setting-${setting.key}`}
      {@const fieldError = fieldErrors[setting.key]}
      {@const dirty = valueOf(setting) !== setting.value}
      <form class="row" onsubmit={(event) => save(event, setting)}>
        <div class="text">
          <label for={id}>{label(setting.key)}</label>
          {#if help(setting.key)}<p id="{id}-help">{help(setting.key)}</p>{/if}
          {#if setting.calendar}
            <ul>
              {#each setting.calendar as window (window.window_kind)}
                <li>
                  {stateLabel(window.window_kind)} budgets use {window.time_zone}
                  until {formatDate(
                    window.ends_at
                  )}{#if window.pending_time_zone}, then {window.pending_time_zone}
                    from {formatDate(window.effective_at)}{/if}
                </li>
              {/each}
            </ul>
          {/if}
          <small
            >Last changed {formatDate(setting.updated_at)} by {setting.updated_by}</small
          >
        </div>
        <div class="control">
          {#if SIGN_IN_KEYS.includes(setting.key)}
            <select
              {id}
              value={valueOf(setting)}
              disabled={!editable(setting)}
              aria-describedby="{id}-help"
              onchange={(event) => edit(setting, event.currentTarget.value)}
            >
              <option value="true">On</option>
              <option value="false">Off</option>
            </select>
          {:else if setting.key === LIMITS_OUTAGE_KEY}
            <select
              {id}
              value={isLimitsOutagePolicy(valueOf(setting))
                ? valueOf(setting)
                : 'fail_closed'}
              disabled={!editable(setting)}
              aria-describedby="{id}-help"
              aria-invalid={fieldError ? 'true' : undefined}
              onchange={(event) => edit(setting, event.currentTarget.value)}
            >
              {#each LIMITS_OUTAGE_POLICIES as policy (policy)}
                <option value={policy}>{OUTAGE_LABELS[policy]}</option>
              {/each}
            </select>
          {:else if isRetentionKey(setting.key)}
            <span class="suffix">
              <input
                {id}
                type="number"
                inputmode="numeric"
                min={RETENTION_MIN_DAYS}
                max={RETENTION_MAX_DAYS}
                step="1"
                value={valueOf(setting)}
                readonly={!editable(setting)}
                aria-describedby="{id}-help"
                aria-invalid={fieldError ? 'true' : undefined}
                oninput={(event) => edit(setting, event.currentTarget.value)}
              />
              <span aria-hidden="true">days</span>
            </span>
          {:else}
            <input
              {id}
              value={valueOf(setting)}
              readonly={!editable(setting)}
              aria-describedby="{id}-help"
              aria-invalid={fieldError ? 'true' : undefined}
              oninput={(event) => edit(setting, event.currentTarget.value)}
            />
          {/if}
          {#if fieldError}<small class="field-error" role="alert"
              >{fieldError}</small
            >{/if}
          {#if dirty}
            <div class="actions">
              <button
                class="button button-secondary"
                type="button"
                onclick={() => discard(setting.key)}>Cancel</button
              >
              <button
                class="button button-primary"
                type="submit"
                disabled={Boolean(savingKey)}
                >{savingKey === setting.key ? 'Saving…' : 'Save'}</button
              >
            </div>
          {/if}
        </div>
      </form>
    {/each}
  </div>
  {#if shown.some((setting) => !editable(setting))}
    <p class="read-only-note">
      Your role can see these settings but not change them.
    </p>
  {/if}
{/if}

<style>
  .settings {
    padding: 0;
  }
  .row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(14rem, 20rem);
    gap: 1rem 2rem;
    padding: 1.25rem 1.5rem;
  }
  .row + .row {
    border-top: 1px solid var(--border-hairline);
  }
  label {
    color: var(--foreground);
    font-weight: 500;
  }
  .text p,
  .text li {
    margin-top: 0.35rem;
    color: var(--foreground-subtle);
    font-size: var(--text-body-sm);
  }
  .text ul {
    margin: 0;
    padding-left: 1.1rem;
  }
  .text small {
    display: block;
    margin-top: 0.5rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .control {
    display: grid;
    align-content: start;
    gap: 0.6rem;
  }
  .suffix {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    color: var(--foreground-muted);
  }
  .suffix input {
    max-width: 8rem;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
  }
  .field-error {
    color: var(--danger);
  }
  @media (max-width: 48rem) {
    .row {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
