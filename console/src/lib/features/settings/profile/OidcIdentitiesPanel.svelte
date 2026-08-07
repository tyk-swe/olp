<script lang="ts">
  import type { OidcIdentityList } from '$lib/api/operations';
  import { formatDate } from '$lib/features/operations/format';
  import type { PendingIdentityAction } from './recentAuthentication';

  type Props = {
    identities: OidcIdentityList | undefined;
    pending: boolean;
    failed: boolean;
    pendingAction: PendingIdentityAction | undefined;
    busy: string;
    error: string;
    onRetry: () => void;
    onCompletePending: () => void | Promise<void>;
    onLink: () => void | Promise<void>;
    onUnlink: (id: string, issuer: string) => void | Promise<void>;
  };

  let {
    identities,
    pending,
    failed,
    pendingAction,
    busy,
    error,
    onRetry,
    onCompletePending,
    onLink,
    onUnlink
  }: Props = $props();
</script>

<section
  class="card panel oidc-panel"
  aria-labelledby="linked-identities-title"
>
  <div>
    <p class="eyebrow">Federated authentication</p>
    <h2 id="linked-identities-title">Linked OIDC identities</h2>
  </div>
  {#if pending}
    <p role="status">Loading linked identities…</p>
  {:else if failed}
    <p class="field-error" role="alert">
      Linked identities are unavailable.
      <button class="text-button" type="button" onclick={onRetry}
        >Try again</button
      >
    </p>
  {:else}
    {#if identities?.data.length}
      <div class="identity-list">
        {#each identities.data as identity (identity.id)}
          <article class="identity-row">
            <div>
              <strong>{identity.email_at_link ?? 'OIDC identity'}</strong>
              <small>{identity.issuer}</small>
              <small
                >{identity.last_login_at
                  ? `Last used ${formatDate(identity.last_login_at)}`
                  : `Linked ${formatDate(identity.created_at)}`}</small
              >
            </div>
            <button
              class="button button-secondary"
              type="button"
              onclick={() => onUnlink(identity.id, identity.issuer)}
              disabled={!identity.can_unlink || Boolean(busy)}
              title={identity.can_unlink
                ? 'Unlink this identity'
                : 'Add another authentication method before unlinking'}
              >{busy === identity.id ? 'Unlinking…' : 'Unlink'}</button
            >
          </article>
        {/each}
      </div>
    {:else}
      <p class="security-note">No OIDC identity is linked to this account.</p>
    {/if}
    {#if pendingAction?.purpose === 'oidc_link'}
      <p class="security-note">
        Your fresh identity verification is ready. Confirm to continue to your
        identity provider.
      </p>
      <button
        class="button button-primary"
        type="button"
        onclick={onCompletePending}
        disabled={Boolean(busy)}
        >{busy === 'link'
          ? 'Redirecting…'
          : 'Confirm OIDC identity link'}</button
      >
    {/if}
    {#if pendingAction?.purpose === 'oidc_unlink'}
      <p class="security-note">
        Your fresh identity verification is ready. Confirm the selected OIDC
        identity unlink.
      </p>
      <button
        class="button button-secondary danger-button"
        type="button"
        onclick={onCompletePending}
        disabled={Boolean(busy)}
        >{busy === pendingAction.resourceId
          ? 'Unlinking…'
          : 'Confirm OIDC identity unlink'}</button
      >
    {/if}
    {#if identities?.linking_available}<button
        class="button button-secondary"
        type="button"
        onclick={onLink}
        disabled={Boolean(busy)}
        >{busy === 'link' ? 'Redirecting…' : 'Link an OIDC identity'}</button
      >{/if}
  {/if}
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  <p class="security-note">
    The final sign-in method cannot be removed, so OIDC-only accounts stay
    recoverable.
  </p>
</section>

<style>
  .panel {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  .oidc-panel {
    grid-column: 1 / -1;
  }
  .identity-list {
    display: grid;
    gap: 0.65rem;
  }
  .identity-row {
    display: flex;
    min-width: 0;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .identity-row > div {
    display: grid;
    min-width: 0;
    gap: 0.15rem;
  }
  .identity-row small {
    overflow-wrap: anywhere;
    color: var(--foreground-muted);
  }
  h2 {
    margin: 0;
    font-size: 1.2rem;
  }
  .field-error {
    margin: 0;
    color: var(--danger);
    font-weight: 700;
  }
  .security-note {
    margin: 0;
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
  .danger-button {
    color: var(--danger);
  }
  .text-button {
    min-height: 2.75rem;
    border: 0;
    background: transparent;
    color: var(--accent-strong);
    font-weight: 700;
  }
  @media (max-width: 40rem) {
    .panel {
      padding: 0.85rem;
    }
    .identity-row {
      display: grid;
    }
  }
</style>
