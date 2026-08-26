<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { isEtagMismatch } from '$lib/api/http';
  import {
    beginOidcLink,
    getOidcConfiguration,
    putOidcConfiguration,
    type OidcConfigurationInput
  } from '$lib/api/management/oidc';
  import {
    beginOidcReauthentication,
    listOidcIdentities,
    reauthenticateWithPassword
  } from '$lib/api/profile';
  import { FIXED_ROLES } from '$lib/auth/authorization';
  import { useRole } from '$lib/auth/useRole.svelte';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import ReauthenticateDialog from '$lib/components/ReauthenticateDialog.svelte';
  import {
    beginReload,
    conflictNotice,
    initialConcurrentEdit,
    markConflict,
    markDirty,
    markSaved,
    reconcile
  } from '$lib/forms/concurrentEdit';
  import { errorMessage as accessErrorMessage } from '$lib/api/http';
  import { parseRoleMappings } from './mappings';

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('users.manage'));
  const oidc = createQuery(() => ({
    queryKey: ['oidc-configuration'],
    queryFn: ({ signal }) => getOidcConfiguration(signal),
    retry: false
  }));

  let sync = $state(initialConcurrentEdit());
  let discoveryUrl = $state('');
  let issuer = $state('');
  let clientId = $state('');
  let clientSecret = $state('');
  let enabled = $state(false);
  let scopes = $state('openid profile email');
  let emailClaim = $state('email');
  let groupsClaim = $state('groups');
  let defaultRole = $state('viewer');
  let emailMappings = $state('');
  let groupMappings = $state('');
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  let reauthenticating = $state(false);
  let reauthenticationBusy = $state(false);
  let reauthenticationError = $state('');
  const concurrentNotice = $derived(conflictNotice(sync));

  $effect(() => {
    if (!oidc.isFetched) return;
    const value = oidc.data;
    const next = reconcile(sync, value?.etag ?? 'new');
    if (next.state !== sync) sync = next.state;
    if (!next.hydrate) return;
    clientSecret = '';
    if (!value) {
      discoveryUrl = '';
      issuer = '';
      clientId = '';
      enabled = false;
      scopes = 'openid profile email';
      emailClaim = 'email';
      groupsClaim = 'groups';
      defaultRole = 'viewer';
      emailMappings = '';
      groupMappings = '';
      return;
    }
    discoveryUrl = value.discovery_url;
    issuer = value.issuer;
    clientId = value.client_id;
    enabled = value.enabled;
    scopes = value.scopes.join(' ');
    emailClaim = value.email_claim;
    groupsClaim = value.groups_claim;
    defaultRole = value.default_role ?? '';
    emailMappings = value.email_role_mappings
      .map((mapping) => `${mapping.claim_value}=${mapping.role}`)
      .join('\n');
    groupMappings = value.group_role_mappings
      .map((mapping) => `${mapping.claim_value}=${mapping.role}`)
      .join('\n');
  });

  onDestroy(() => {
    clientSecret = '';
  });

  function touch() {
    sync = markDirty(sync);
  }

  async function reload() {
    const result = await oidc.refetch();
    if (result.error) return;
    sync = beginReload(sync);
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage) return;
    error = notice = '';
    if (!discoveryUrl || !issuer || !clientId) {
      error = 'Issuer, discovery URL, and client ID are required.';
      return;
    }

    busy = 'oidc-save';
    try {
      const input: OidcConfigurationInput = {
        discovery_url: discoveryUrl.trim(),
        issuer: issuer.trim(),
        client_id: clientId.trim(),
        client_secret: clientSecret || null,
        enabled,
        scopes: scopes.split(/[ ,]+/).filter(Boolean),
        email_claim: emailClaim.trim() || 'email',
        groups_claim: groupsClaim.trim() || 'groups',
        default_role: defaultRole || null,
        email_role_mappings: parseRoleMappings(emailMappings),
        group_role_mappings: parseRoleMappings(groupMappings)
      };
      const etag = sync.snapshotEtag;
      const updated = await putOidcConfiguration(
        input,
        etag && etag !== 'new' ? etag : undefined
      );
      clientSecret = '';
      sync = markSaved(sync, updated.etag);
      queryClient.setQueryData(['oidc-configuration'], updated);
      notice = updated.enabled
        ? 'OIDC configuration validated and enabled.'
        : 'OIDC configuration saved but disabled.';
    } catch (cause) {
      if (isEtagMismatch(cause)) sync = markConflict(sync);
      else error = accessErrorMessage(cause);
    } finally {
      busy = '';
    }
  }

  async function linkIdentity() {
    busy = 'oidc-link';
    error = notice = '';
    try {
      const identities = await listOidcIdentities();
      if (!identities.has_local_password) {
        // The profile callback requires explicit confirmation before consuming
        // the one-time OIDC grant.
        window.location.assign(await beginOidcReauthentication('oidc_link'));
        return;
      }
      reauthenticationError = '';
      reauthenticating = true;
    } catch (cause) {
      error = accessErrorMessage(cause);
    } finally {
      busy = '';
    }
  }

  async function confirmLinkIdentity(password: string) {
    reauthenticationBusy = true;
    reauthenticationError = '';
    try {
      await reauthenticateWithPassword(password, 'oidc_link');
      reauthenticating = false;
      window.location.assign(await beginOidcLink());
    } catch (cause) {
      reauthenticationError = accessErrorMessage(cause);
    } finally {
      reauthenticationBusy = false;
    }
  }
</script>

{#if reauthenticating}
  <ReauthenticateDialog
    title="Confirm the OIDC link"
    description="Linking an OIDC identity changes how you sign in, so confirm your current password first."
    busy={reauthenticationBusy}
    error={reauthenticationError}
    onConfirm={confirmLinkIdentity}
    onCancel={() => (reauthenticating = false)}
  />
{/if}

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

{#if oidc.isPending}
  <div class="loading-state" role="status">Loading OIDC configuration…</div>
{:else if oidc.isError}
  <div class="inline-problem" role="alert">
    {accessErrorMessage(oidc.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => oidc.refetch()}>Retry</button
    >
  </div>
{:else}
  {#if !canManage}
    <ReadOnlyNote>
      Your role can view the OIDC configuration but not change it.
    </ReadOnlyNote>
  {/if}
  <ConflictNotice
    notice={concurrentNotice}
    onReload={reload}
    disabled={Boolean(busy)}
  />
  <form class="oidc-grid" onsubmit={save} oninput={touch} onchange={touch}>
    <section class="card oidc-form" aria-labelledby="oidc-heading">
      <div class="section-heading">
        <div>
          <p class="eyebrow">Single identity provider</p>
          <h2 id="oidc-heading">OIDC Authorization Code + PKCE</h2>
        </div>
        <label class="enabled"
          ><input
            type="checkbox"
            bind:checked={enabled}
            disabled={!canManage}
          /> Enabled</label
        >
      </div>
      <p class="muted">
        Discovery metadata is validated server-side and must match the issuer
        configured with your identity provider. Every flow uses PKCE, state, and
        nonce; identities require explicit linking.
      </p>
      {#if oidc.data}<p class="muted">
          Updated by {oidc.data.updated_by_email ?? 'a removed account'}
        </p>{/if}
      <div class="form-grid">
        <div class="form-field full">
          <label for="oidc-issuer">Expected issuer</label><input
            id="oidc-issuer"
            type="url"
            bind:value={issuer}
            placeholder="https://id.example.com"
            disabled={!canManage}
            required
          />
        </div>
        <div class="form-field full">
          <label for="discovery-url">Discovery URL</label><input
            id="discovery-url"
            type="url"
            bind:value={discoveryUrl}
            placeholder="https://id.example.com/.well-known/openid-configuration"
            disabled={!canManage}
            required
          />
        </div>
        <div class="form-field">
          <label for="client-id">Client ID</label><input
            id="client-id"
            autocomplete="off"
            bind:value={clientId}
            disabled={!canManage}
            required
          />
        </div>
        <div class="form-field">
          <label for="client-secret">Client secret</label><input
            id="client-secret"
            type="password"
            autocomplete="new-password"
            bind:value={clientSecret}
            disabled={!canManage}
            placeholder={oidc.data?.has_client_secret
              ? 'Leave blank to keep current secret'
              : 'Write-only secret'}
          />
        </div>
        <div class="form-field full">
          <label for="oidc-scopes">Scopes</label><input
            id="oidc-scopes"
            bind:value={scopes}
            disabled={!canManage}
          />
        </div>
        <div class="form-field">
          <label for="email-claim">Email claim</label><input
            id="email-claim"
            bind:value={emailClaim}
            disabled={!canManage}
          />
        </div>
        <div class="form-field">
          <label for="groups-claim">Groups claim</label><input
            id="groups-claim"
            bind:value={groupsClaim}
            disabled={!canManage}
          />
        </div>
        <div class="form-field">
          <label for="default-role">Default role</label><select
            id="default-role"
            bind:value={defaultRole}
            disabled={!canManage}
            ><option value="">No default (mapping required)</option
            >{#each FIXED_ROLES as role (role)}<option value={role}
                >{role}</option
              >{/each}</select
          >
        </div>
      </div>
    </section>
    <section class="card mapping-form" aria-labelledby="mapping-heading">
      <p class="eyebrow">Authorization mapping</p>
      <h2 id="mapping-heading">Claims to fixed roles</h2>
      <p class="muted">
        One mapping per line in <code>claim-value=role</code> form. Email mappings
        take precedence over group mappings and the default.
      </p>
      <div class="form-field">
        <label for="email-mappings">Email mappings</label><textarea
          id="email-mappings"
          bind:value={emailMappings}
          disabled={!canManage}
          placeholder="owner@example.com=owner"></textarea>
      </div>
      <div class="form-field">
        <label for="group-mappings">Group mappings</label><textarea
          id="group-mappings"
          bind:value={groupMappings}
          disabled={!canManage}
          placeholder="platform-team=operator"></textarea>
      </div>
      {#if !oidc.data?.enabled}
        <ReadOnlyNote role={undefined}>
          Save with OIDC enabled before linking your identity.
        </ReadOnlyNote>
      {/if}
      <div class="oidc-actions">
        <button
          class="button button-secondary"
          type="button"
          onclick={linkIdentity}
          disabled={!oidc.data?.enabled || Boolean(busy)}
          >{busy === 'oidc-link' ? 'Redirecting…' : 'Link my identity'}</button
        ><button
          class="button button-primary"
          type="submit"
          disabled={!canManage || Boolean(busy)}
          >{busy === 'oidc-save' ? 'Validating…' : 'Save and validate'}</button
        >
      </div>
    </section>
  </form>
{/if}

<style>
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  h2 {
    margin: 0;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }
  .muted {
    color: var(--foreground-muted);
  }
  .oidc-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.2fr) minmax(22rem, 0.8fr);
    gap: 1rem;
  }
  .oidc-form,
  .mapping-form {
    padding: clamp(1.15rem, 3vw, 1.5rem);
  }
  .section-heading {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 1rem;
  }
  .enabled {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
    font-weight: 700;
  }
  .mapping-form {
    display: grid;
    align-content: start;
    gap: 1rem;
  }
  .mapping-form h2,
  .mapping-form p {
    margin: 0;
  }
  .oidc-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.65rem;
  }
  code {
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
  }
  @media (max-width: 64rem) {
    .oidc-grid {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 42rem) {
    .oidc-actions {
      display: grid;
    }
  }
</style>
