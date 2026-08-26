<script lang="ts">
  import { PASSWORD_TOO_SHORT } from '$lib/passwordPolicy';

  type Props = {
    identitiesPending: boolean;
    identitiesError: boolean;
    enrollmentNeeded: boolean;
    enrollmentGrantReady: boolean;
    currentPassword: string;
    newPassword: string;
    confirmPassword: string;
    passwordError: string;
    saving: boolean;
    onRetryIdentities: () => void;
    onVerifyEnrollment: () => void | Promise<void>;
    onSave: (event: SubmitEvent) => void | Promise<void>;
  };

  let {
    identitiesPending,
    identitiesError,
    enrollmentNeeded,
    enrollmentGrantReady,
    currentPassword = $bindable(),
    newPassword = $bindable(),
    confirmPassword = $bindable(),
    passwordError,
    saving,
    onRetryIdentities,
    onVerifyEnrollment,
    onSave
  }: Props = $props();
</script>

<form class="card panel" aria-busy={identitiesPending} onsubmit={onSave}>
  <div>
    <p class="eyebrow">Local authentication</p>
    <h2>
      {identitiesPending || identitiesError
        ? 'Local password'
        : enrollmentNeeded
          ? 'Add a local password'
          : 'Change password'}
    </h2>
  </div>
  {#if identitiesPending}
    <p role="status">Checking your sign-in methods…</p>
  {:else if identitiesError}
    <p class="field-error" role="alert">
      Your sign-in methods are unavailable.
      <button class="text-button" type="button" onclick={onRetryIdentities}
        >Try again</button
      >
    </p>
  {:else if enrollmentNeeded && !enrollmentGrantReady}
    <p class="security-note">
      A fresh identity-provider sign-in is required before adding a durable
      local credential.
    </p>
    {#if passwordError}<p id="password-error" class="field-error" role="alert">
        {passwordError}
      </p>{/if}
    <button
      class="button button-primary"
      type="button"
      onclick={onVerifyEnrollment}
      disabled={saving}
      >{saving ? 'Redirecting…' : 'Verify identity with OIDC'}</button
    >
  {:else}
    {#if !enrollmentNeeded}<div class="form-field">
        <label for="current-password">Current password</label>
        <input
          id="current-password"
          bind:value={currentPassword}
          type="password"
          autocomplete="current-password"
          aria-invalid={Boolean(passwordError)}
          aria-describedby={passwordError ? 'password-error' : undefined}
        />
      </div>{/if}
    <div class="form-field">
      <label for="new-password">New password</label>
      <input
        id="new-password"
        bind:value={newPassword}
        type="password"
        autocomplete="new-password"
        aria-invalid={Boolean(passwordError)}
        aria-describedby={passwordError
          ? 'new-password-help password-error'
          : 'new-password-help'}
      />
      <small id="new-password-help">{PASSWORD_TOO_SHORT}</small>
    </div>
    <div class="form-field">
      <label for="confirm-password">Confirm new password</label>
      <input
        id="confirm-password"
        bind:value={confirmPassword}
        type="password"
        autocomplete="new-password"
        aria-invalid={Boolean(passwordError)}
        aria-describedby={passwordError ? 'password-error' : undefined}
      />
    </div>
    {#if passwordError}<p id="password-error" class="field-error" role="alert">
        {passwordError}
      </p>{/if}
    <button class="button button-primary" type="submit" disabled={saving}
      >{saving
        ? 'Saving…'
        : enrollmentNeeded
          ? 'Add local password'
          : 'Change password'}</button
    >
    <p class="security-note">
      This change revokes every previous session and rotates this browser to a
      new session.
    </p>
  {/if}
</form>

<style>
  .panel {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  h2 {
    margin: 0;
    font-size: 1.2rem;
  }
  .form-field input {
    width: 100%;
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
  }
</style>
