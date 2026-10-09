<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { page } from '$app/state';
  import { errorMessage } from '$lib/api/http';
  import { authLifecycle } from '../session/lifecycle';
  import {
    reauthenticateWithPassword,
    beginIdentityReauthentication
  } from '../profile/api';
  import { listSAMLIdentities, beginSAMLLink, unlinkSAML } from './api';
  let { hasLocalPassword = false } = $props<{ hasLocalPassword?: boolean }>();
  const client = useQueryClient();
  const key = ['profile', 'saml'];
  const identities = createQuery(() => ({
    queryKey: key,
    queryFn: ({ signal }) => listSAMLIdentities(signal)
  }));
  let password = $state(''),
    busy = $state(false),
    error = $state(''),
    notice = $state('');
  async function act(kind: 'saml_link' | 'saml_unlink', id?: string) {
    if (busy) return;
    busy = true;
    error = '';
    notice = '';
    try {
      const verified =
        page.url.searchParams.get('reauthenticated') === kind &&
        (kind === 'saml_link' ||
          page.url.searchParams.get('resource_id') === id);
      if (!verified) {
        if (hasLocalPassword) {
          await reauthenticateWithPassword(password, kind, id);
          password = '';
        } else {
          window.location.assign(await beginIdentityReauthentication(kind, id));
          return;
        }
      }
      if (kind === 'saml_link') window.location.assign(await beginSAMLLink());
      else if (id) {
        await unlinkSAML(id);
        await authLifecycle.validateSession();
        await client.invalidateQueries({ queryKey: ['profile'] });
        notice = 'SAML identity unlinked. Previous sessions were revoked.';
      }
    } catch (e) {
      error = errorMessage(
        e,
        'The SAML identity operation could not be completed.'
      );
    } finally {
      busy = false;
    }
  }
</script>

<section class="card" aria-label="SAML identities">
  <h2>Linked SAML identities</h2>
  <p>
    Linking requires fresh authentication and a new signed identity-provider
    response. An email match alone never links an account. Unlinking must leave
    another usable sign-in method.
  </p>
  {#if error}<p class="form-alert" role="alert">{error}</p>{/if}{#if notice}<p
      role="status"
    >
      {notice}
    </p>{/if}
  {#if identities.isPending}<p role="status">
      Loading SAML identities…
    </p>{:else if identities.isError}<p role="alert">
      {errorMessage(identities.error, 'SAML identities are unavailable.')}
    </p>
    <button class="button button-secondary" onclick={() => identities.refetch()}
      >Retry</button
    >{:else}
    {#if hasLocalPassword}<div class="form-field">
        <label for="saml-identity-password"
          >Current password for SAML changes</label
        ><input
          id="saml-identity-password"
          type="password"
          autocomplete="current-password"
          bind:value={password}
          disabled={busy}
        />
      </div>{/if}
    {#each identities.data?.items ?? [] as identity (identity.id)}<div
        class="identity"
      >
        <div>
          <strong>{identity.email_at_link}</strong>
          <p>{identity.issuer}</p>
          <small>Subject: {identity.subject}</small>
        </div>
        <button
          class="button button-secondary"
          disabled={busy}
          onclick={() => act('saml_unlink', identity.id)}
          >Unlink SAML identity</button
        >
      </div>{:else}<p>No SAML identities linked.</p>{/each}
    {#if identities.data?.linking_available}<button
        class="button button-primary"
        disabled={busy}
        onclick={() => act('saml_link')}>Link SAML identity</button
      >{/if}{/if}
</section>

<style>
  section {
    padding: 1.25rem;
    display: grid;
    gap: 1rem;
  }
  h2 {
    font-weight: 500;
  }
  .identity {
    display: flex;
    flex-wrap: wrap;
    gap: 1rem;
    justify-content: space-between;
    align-items: center;
    border-top: 1px solid var(--border);
    padding-top: 1rem;
  }
  p,
  small {
    overflow-wrap: anywhere;
  }
</style>
