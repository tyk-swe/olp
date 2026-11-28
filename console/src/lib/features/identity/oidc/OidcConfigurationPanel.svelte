<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { oidcKeys } from '#lib/features/identity/oidc/oidcKeys.js';

  import { onDestroy } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { isEtagMismatch } from '#lib/management/http.js';
  import {
    beginOidcLink,
    getOidcConfiguration,
    putOidcConfiguration
  } from '#lib/features/identity/oidc/api.js';
  import {
    beginIdentityReauthentication,
    listOidcIdentities
  } from '#lib/features/identity/profile/api.js';
  import { reauthenticateWithPassword } from '#lib/features/identity/profile/reauthentication.js';
  import { FIXED_ROLES } from '#lib/session/authorization.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import { focusFormError } from '#lib/editing/focusError.js';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import ReauthenticateDialog from '#lib/features/identity/profile/ReauthenticateDialog.svelte';
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
  import {
    oidcDraft,
    oidcServerFields,
    validateOidc,
    type OidcField
  } from './oidcDraft';

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('users.manage'));
  const oidc = createQuery(() => ({
    queryKey: oidcKeys.configuration(),
    queryFn: ({ signal }) => getOidcConfiguration(signal),
    retry: false
  }));

  let sync = $state(initialConcurrentEdit());
  let draft = $state(oidcDraft());
  let editVersion = 0;
  const write = useAction<OidcField>();
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
    draft = oidcDraft(value);
  });

  onDestroy(() => {
    draft.clientSecret = '';
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
    const form = event.currentTarget as HTMLFormElement;
    if (!canManage || write.busy) return;
    notice = '';
    const checked = validateOidc(draft);
    if (!write.check(checked)) return focusFormError(form);

    const submittedVersion = editVersion;
    const submittedClientSecret = draft.clientSecret;
    const etag = sync.snapshotEtag;
    const saved = await write.run(
      () =>
        putOidcConfiguration(
          checked.value,
          etag && etag !== 'new' ? etag : undefined
        ),
      (updated) => {
        if (draft.clientSecret === submittedClientSecret)
          draft.clientSecret = '';
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
        serverFields: oidcServerFields,
        failure: (cause) => {
          if (isEtagMismatch(cause)) {
            sync = markConflict(sync);
            return '';
          }
          return errorMessage(cause);
        }
      }
    );
    if (!saved) await focusFormError(form);
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
        reauth.reset();
        reauthenticating = true;
      },
      { key: 'oidc-link', failure: (cause) => errorMessage(cause) }
    );
  }

  async function confirmLinkIdentity(password: string) {
    const verified = await reauth.run(
      () => reauthenticateWithPassword(password, 'oidc_link'),
      () => {
        reauthenticating = false;
      },
      { failure: (cause) => errorMessage(cause) }
    );
    if (verified)
      await write.run(
        beginOidcLink,
        (url) => {
          window.location.assign(url);
        },
        { key: 'oidc-link', failure: (cause) => errorMessage(cause) }
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
  <form
    class="oidc-grid"
    onsubmit={save}
    oninput={touch}
    onchange={touch}
    novalidate
  >
    {#if write.error}<div
        class="inline-problem"
        role="alert"
        tabindex="-1"
        data-error-summary
      >
        {write.error}
      </div>{/if}
    <section class="card oidc-form" aria-labelledby="oidc-heading">
      <div class="section-heading">
        <div>
          <h2 id="oidc-heading">OpenID Connect</h2>
        </div>
        <label class="enabled">
          <input
            type="checkbox"
            bind:checked={draft.enabled}
            disabled={!canManage}
          /> Enabled
        </label>
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
        <FormField
          label="Expected issuer"
          error={write.fieldErrors.issuer}
          full
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              type="url"
              bind:value={draft.issuer}
              placeholder="https://id.example.com"
              disabled={!canManage}
              required
            />
          {/snippet}
        </FormField>
        <FormField
          label="Discovery URL"
          error={write.fieldErrors.discoveryUrl}
          full
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              type="url"
              bind:value={draft.discoveryUrl}
              placeholder="https://id.example.com/.well-known/openid-configuration"
              disabled={!canManage}
              required
            />
          {/snippet}
        </FormField>
        <FormField label="Client ID" error={write.fieldErrors.clientId}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              autocomplete="off"
              bind:value={draft.clientId}
              disabled={!canManage}
              required
            />
          {/snippet}
        </FormField>
        <FormField label="Client secret" error={write.fieldErrors.clientSecret}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              type="password"
              autocomplete="new-password"
              bind:value={draft.clientSecret}
              disabled={!canManage}
              placeholder={oidc.data?.has_client_secret
                ? 'Leave blank to keep current secret'
                : 'Write-only secret'}
            />
          {/snippet}
        </FormField>
        <FormField label="Scopes" error={write.fieldErrors.scopes} full>
          {#snippet control(attributes)}
            <input
              {...attributes}
              bind:value={draft.scopes}
              disabled={!canManage}
            />
          {/snippet}
        </FormField>
        <FormField label="Email claim" error={write.fieldErrors.emailClaim}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              bind:value={draft.emailClaim}
              disabled={!canManage}
            />
          {/snippet}
        </FormField>
        <FormField label="Groups claim" error={write.fieldErrors.groupsClaim}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              bind:value={draft.groupsClaim}
              disabled={!canManage}
            />
          {/snippet}
        </FormField>
        <FormField label="Default role" error={write.fieldErrors.defaultRole}>
          {#snippet control(attributes)}
            <select
              {...attributes}
              bind:value={draft.defaultRole}
              disabled={!canManage}
            >
              <option value="">
                No default (mapping required)
              </option>{#each FIXED_ROLES as role (role)}<option value={role}>
                  {role}
                </option>{/each}
            </select>
          {/snippet}
        </FormField>
      </div>
    </section>
    <OidcRoleMappings
      bind:emailMappings={draft.emailMappings}
      bind:groupMappings={draft.groupMappings}
      errors={write.fieldErrors}
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
  .oidc-grid > .inline-problem {
    grid-column: 1 / -1;
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
