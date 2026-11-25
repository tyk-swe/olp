<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { page } from '$app/state';
  import { errorMessage } from '#lib/management/http.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { profileKeys } from '../profile/profileKeys';
  import {
    reauthenticateWithPassword,
    beginIdentityReauthentication
  } from '../profile/api';
  import { listSAMLIdentities, beginSAMLLink, unlinkSAML } from './api';
  type SamlOutcome = { redirect: string } | { unlinked: boolean };
  let { hasLocalPassword = false }: { hasLocalPassword?: boolean } = $props();
  const client = useQueryClient();
  const identities = createQuery(() => ({
    queryKey: profileKeys.saml(),
    queryFn: ({ signal }) => listSAMLIdentities(signal)
  }));
  const saml = useAction();
  let password = $state(''),
    notice = $state('');
  async function act(kind: 'saml_link' | 'saml_unlink', id?: string) {
    notice = '';
    await saml.run(
      async (): Promise<SamlOutcome> => {
        const verified =
          page.url.searchParams.get('reauthenticated') === kind &&
          (kind === 'saml_link' ||
            page.url.searchParams.get('resource_id') === id);
        if (!verified) {
          if (hasLocalPassword) {
            await reauthenticateWithPassword(password, kind, id);
            password = '';
          } else {
            return { redirect: await beginIdentityReauthentication(kind, id) };
          }
        }
        if (kind === 'saml_link') return { redirect: await beginSAMLLink() };
        if (id) {
          await unlinkSAML(id);
          return { unlinked: true };
        }
        return { unlinked: false };
      },
      async (outcome, current) => {
        if ('redirect' in outcome) {
          window.location.assign(outcome.redirect);
          return;
        }
        if (!outcome.unlinked) return;
        await authLifecycle.validateSession();
        await client.invalidateQueries({ queryKey: profileKeys.current() });
        if (!current()) return;
        notice = 'SAML identity unlinked. Previous sessions were revoked.';
      },
      { fallback: 'The SAML identity operation could not be completed.' }
    );
  }
</script>

<section class="card" aria-label="SAML identities">
  <h2>Linked SAML identities</h2>
  <p>
    Linking requires fresh authentication and a new signed identity-provider
    response. An email match alone never links an account. Unlinking must leave
    another usable sign-in method.
  </p>
  {#if saml.error}<p class="inline-problem" role="alert">
      {saml.error}
    </p>{/if}{#if notice}<Callout tone="success">{notice}</Callout>{/if}
  <QueryView
    query={identities}
    loading="Loading SAML identities…"
    failure={errorMessage(identities.error, 'SAML identities are unavailable.')}
  >
    {#if hasLocalPassword}<div class="form-field">
        <label for="saml-identity-password">
          Current password for SAML changes
        </label><input
          id="saml-identity-password"
          type="password"
          autocomplete="current-password"
          bind:value={password}
          disabled={saml.busy}
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
          disabled={saml.busy}
          onclick={() => act('saml_unlink', identity.id)}
        >
          Unlink SAML identity
        </button>
      </div>{:else}<p>No SAML identities linked.</p>{/each}
    {#if identities.data?.linking_available}<button
        class="button button-primary"
        disabled={saml.busy}
        onclick={() => act('saml_link')}
      >
        Link SAML identity
      </button>{/if}
  </QueryView>
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
