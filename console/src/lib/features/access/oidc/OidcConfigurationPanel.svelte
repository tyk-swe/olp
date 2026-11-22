<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { oidcKeys } from '#lib/features/access/oidc/oidcKeys.js';

  import { onDestroy } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { isEtagMismatch } from '#lib/management/http.js';
  import {
    beginOidcLink,
    getOidcConfiguration,
    putOidcConfiguration,
    type OidcConfigurationInput
  } from '#lib/features/access/oidc/api.js';
  import {
    beginIdentityReauthentication,
    listOidcIdentities
  } from '#lib/features/access/profile/api.js';
  import { reauthenticateWithPassword } from '#lib/features/access/profile/reauthentication.js';
  import { FIXED_ROLES } from '#lib/session/authorization.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import ConflictNotice from '#lib/shell/ConflictNotice.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import ReauthenticateDialog from '#lib/shell/ReauthenticateDialog.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import OidcRoleMappings from './OidcRoleMappings.svelte';
  import {
    beginReload,
    conflictNotice,
    initialConcurrentEdit,
    markConflict,
    markDirty,
    markSaved,
    reconcile
  } from '#lib/editing/concurrentEdit.js';
  import { errorMessage } from '#lib/management/http.js';
  import { parseRoleMappings } from '#lib/features/access/oidc/mappings.js';

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('users.manage'));
  const oidc = createQuery(() => ({
    queryKey: oidcKeys.configuration(),
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
  let editVersion = 0;
  const write = useAction();
  const reauth = useAction();
  let notice = $state('');
  let reauthenticating = $state(false);
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
    editVersion += 1;
    sync = markDirty(sync);
  }

  async function reload() {
    const result = await oidc.refetch();
    if (result.error) return;
    sync = beginReload(sync);
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || write.busy) return;
    notice = '';
    if (!discoveryUrl || !issuer || !clientId) {
      write.error = 'Issuer, discovery URL, and client ID are required.';
      return;
    }

    const submittedVersion = editVersion;
    const submittedClientSecret = clientSecret;
    await write.run(
      () => {
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
        return putOidcConfiguration(
          input,
          etag && etag !== 'new' ? etag : undefined
        );
      },
      (updated) => {
        if (clientSecret === submittedClientSecret) clientSecret = '';
        sync = markSaved(updated.etag, editVersion !== submittedVersion);
        queryClient.setQueryData(oidcKeys.configuration(), updated);
        const savedNotice = updated.enabled
          ? 'OIDC configuration validated and enabled.'
          : 'OIDC configuration saved but disabled.';
        notice = sync.dirty
          ? `${savedNotice} You have additional unsaved changes.`
          : savedNotice;
      },
      {
        key: 'oidc-save',
        failure: (cause) => {
          if (isEtagMismatch(cause)) {
            sync = markConflict(sync);
            return '';
          }
          return errorMessage(cause);
        }
      }
    );
  }

  async function linkIdentity() {
    notice = '';
    await write.run(
      async () => {
        const identities = await listOidcIdentities();
        if (identities.has_local_password) return { reauth: true as const };
        return {
          redirect: await beginIdentityReauthentication('oidc_link')
        };
      },
      (outcome) => {
        if ('redirect' in outcome) {
          if (outcome.redirect !== undefined)
            window.location.assign(outcome.redirect);
          return;
        }
        reauth.error = '';
        reauthenticating = true;
      },
      { key: 'oidc-link', failure: (cause) => errorMessage(cause) }
    );
  }

  async function confirmLinkIdentity(password: string) {
    write.error = '';
    await reauth.run(
      () => reauthenticateWithPassword(password, 'oidc_link'),
      async () => {
        reauthenticating = false;
        window.location.assign(await beginOidcLink());
      },
      {
        failure: (cause) => {
          const detail = errorMessage(cause);
          if (!reauthenticating) {
            write.error = detail;
            return '';
          }
          return detail;
        }
      }
    );
  }
</script>

{#if reauthenticating}
  <ReauthenticateDialog
    title="Confirm the OIDC link"
    description="Linking an OIDC identity changes how you sign in, so confirm your current password first."
    busy={reauth.busy}
    error={reauth.error}
    onConfirm={confirmLinkIdentity}
    onCancel={() => (reauthenticating = false)}
  />
{/if}

{#if write.error}<div class="inline-problem" role="alert">
    {write.error}
  </div>{/if}
{#if notice}<Callout tone="success">{notice}</Callout>{/if}

<QueryView
  query={oidc}
  loading="Loading OIDC configuration…"
  failure={errorMessage(oidc.error)}
>
  {#if !canManage}
    <ReadOnlyNote>
      Your role can view the OIDC configuration but not change it.
    </ReadOnlyNote>
  {/if}
  <ConflictNotice
    notice={concurrentNotice}
    onReload={reload}
    disabled={write.busy}
  />
  <form class="oidc-grid" onsubmit={save} oninput={touch} onchange={touch}>
    <section class="card oidc-form" aria-labelledby="oidc-heading">
      <div class="section-heading">
        <div>
          <h2 id="oidc-heading">OpenID Connect</h2>
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
    <OidcRoleMappings
      bind:emailMappings
      bind:groupMappings
      {canManage}
      oidcEnabled={Boolean(oidc.data?.enabled)}
      busy={write.busy}
      busyKey={write.key}
      onLinkIdentity={linkIdentity}
    />
  </form>
</QueryView>

<style>
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .oidc-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.2fr) minmax(22rem, 0.8fr);
    gap: 1rem;
  }
  .oidc-form {
    display: grid;
    align-content: start;
    gap: 1rem;
    padding: 1.5rem;
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
    font-weight: 500;
  }
  @media (max-width: 64rem) {
    .oidc-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
