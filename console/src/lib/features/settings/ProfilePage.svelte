<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import {
    beginOidcLink,
    changePassword,
    enrollPassword,
    getProfile,
    listOidcIdentities,
    listSessions,
    revokeSession,
    unlinkOidcIdentity,
    updateProfile
  } from '$lib/api/operations';
  import { clearCsrfToken } from '$lib/api/session';
  import { formatDate } from '$lib/features/operations/format';
  import { validateDisplayName, validateNewPassword, validatePassword } from './validation';

  let displayName = $state('');
  let displayNameError = $state('');
  let currentPassword = $state('');
  let newPassword = $state('');
  let confirmPassword = $state('');
  let profileError = $state('');
  let passwordError = $state('');
  let message = $state('');
  let savingProfile = $state(false);
  let savingPassword = $state(false);
  let revoking = $state('');
  let identityBusy = $state('');
  let identityError = $state('');
  let sessionCursor = $state<string | undefined>();
  let sessionHistory = $state<string[]>([]);

  const profile = createQuery(() => ({
    queryKey: ['profile'],
    queryFn: async () => {
      const data = await getProfile();
      displayName = data.display_name;
      displayNameError = '';
      return data;
    }
  }));
  const sessions = createQuery(() => ({ queryKey: ['profile-sessions', sessionCursor], queryFn: () => listSessions(sessionCursor) }));
  const identities = createQuery(() => ({ queryKey: ['profile-oidc-identities'], queryFn: listOidcIdentities }));
  let passwordEnrollmentNeeded = $derived(identities.data?.data.some((identity) => !identity.can_unlink) ?? false);

  function changeDisplayName(value: string) {
    displayName = value;
    try {
      validateDisplayName(value);
      displayNameError = '';
    } catch (cause) {
      displayNameError = cause instanceof Error ? cause.message : 'Enter a valid display name.';
    }
  }

  async function saveProfile(event: SubmitEvent) {
    event.preventDefault();
    if (!profile.data) return;
    let normalizedDisplayName: string;
    try {
      normalizedDisplayName = validateDisplayName(displayName);
      displayNameError = '';
    } catch (cause) {
      displayNameError = cause instanceof Error ? cause.message : 'Enter a valid display name.';
      return;
    }
    profileError = message = '';
    savingProfile = true;
    try {
      await updateProfile(profile.data, { display_name: normalizedDisplayName });
      message = 'Profile updated.';
      await profile.refetch();
    } catch (cause) {
      profileError = cause instanceof Error ? cause.message : 'The profile could not be updated.';
    } finally {
      savingProfile = false;
    }
  }

  async function savePassword(event: SubmitEvent) {
    event.preventDefault();
    if (!profile.data) return;
    passwordError = message = '';
    savingPassword = true;
    try {
      const next = passwordEnrollmentNeeded
        ? validateNewPassword(newPassword, confirmPassword)
        : validatePassword(currentPassword, newPassword, confirmPassword);
      if (passwordEnrollmentNeeded) {
        await enrollPassword(profile.data, { new_password: next });
      } else {
        await changePassword(profile.data, { current_password: currentPassword, new_password: next });
      }
      currentPassword = newPassword = confirmPassword = '';
      message = passwordEnrollmentNeeded
        ? 'Local password added. Your other sessions were revoked.'
        : 'Password changed. Your other sessions were revoked.';
      await Promise.all([profile.refetch(), sessions.refetch(), identities.refetch()]);
    } catch (cause) {
      passwordError =
        cause instanceof Error
          ? cause.message
          : passwordEnrollmentNeeded
            ? 'The local password could not be added.'
            : 'The password could not be changed.';
    } finally {
      savingPassword = false;
    }
  }

  async function endSession(id: string, current: boolean) {
    revoking = id;
    passwordError = message = '';
    try {
      await revokeSession(id);
      if (current) {
        clearCsrfToken();
        await goto(resolve('/login'), { replaceState: true });
        return;
      }
      message = 'Session revoked.';
      await sessions.refetch();
    } catch (cause) {
      passwordError = cause instanceof Error ? cause.message : 'The session could not be revoked.';
    } finally {
      revoking = '';
    }
  }

  async function linkIdentity() {
    identityBusy = 'link';
    identityError = '';
    try {
      window.location.assign(await beginOidcLink());
    } catch (cause) {
      identityError = cause instanceof Error ? cause.message : 'The OIDC link flow could not start.';
      identityBusy = '';
    }
  }

  async function unlinkIdentity(id: string, issuer: string) {
    if (!confirm(`Unlink the identity from ${issuer}?`)) return;
    identityBusy = id;
    identityError = '';
    try {
      await unlinkOidcIdentity(id);
      message = 'OIDC identity unlinked.';
      await identities.refetch();
    } catch (cause) {
      identityError = cause instanceof Error ? cause.message : 'The OIDC identity could not be unlinked.';
    } finally {
      identityBusy = '';
    }
  }

  function nextSessions() {
    if (!sessions.data?.next_cursor) return;
    sessionHistory = [...sessionHistory, sessions.data.next_cursor];
    sessionCursor = sessions.data.next_cursor;
  }

  function previousSessions() {
    const history = [...sessionHistory];
    history.pop();
    sessionHistory = history;
    sessionCursor = history.at(-1);
  }
</script>

<svelte:head><title>Personal profile · OpenLLMProxy</title></svelte:head>

<div class="page-header"><div><p class="eyebrow">Account</p><h1 class="page-title">Personal profile</h1><p class="page-description">Manage your display name, local password, and signed-in browser sessions.</p></div><a class="button button-secondary" href={resolve('/settings')}>Installation settings</a></div>

{#if message}<p class="success-message" role="status">{message}</p>{/if}

{#if profile.isPending}<div class="loading-state" role="status">Loading your profile…</div>
{:else if profile.isError}<div class="inline-problem" role="alert">Your profile is unavailable. <button class="text-button" onclick={() => profile.refetch()}>Try again</button></div>
{:else if profile.data}
  <div class="profile-grid">
    <form class="card panel" onsubmit={saveProfile}>
      <div><p class="eyebrow">Identity</p><h2>Profile details</h2></div>
      <div class="form-field"><label for="profile-name">Display name</label><input id="profile-name" name="display_name" value={displayName} oninput={(event) => changeDisplayName(event.currentTarget.value)} autocomplete="name" aria-describedby={displayNameError ? 'profile-name-help profile-name-error' : 'profile-name-help'} aria-invalid={Boolean(displayNameError)} /><small id="profile-name-help">Your email and fixed role are managed by an owner.</small>{#if displayNameError}<small id="profile-name-error" class="field-error">{displayNameError}</small>{/if}</div>
      <dl><div><dt>Email</dt><dd>{profile.data.email}</dd></div><div><dt>Role</dt><dd><span class="badge accent">{profile.data.role}</span></dd></div></dl>
      {#if profileError}<p id="profile-error" class="field-error" role="alert">{profileError}</p>{/if}
      <button class="button button-primary" type="submit" disabled={savingProfile}>{savingProfile ? 'Saving…' : 'Save profile'}</button>
    </form>

    <form class="card panel" aria-busy={identities.isPending} onsubmit={savePassword}>
      <div><p class="eyebrow">Local authentication</p><h2>{identities.isPending || identities.isError ? 'Local password' : passwordEnrollmentNeeded ? 'Add a local password' : 'Change password'}</h2></div>
      {#if identities.isPending}
        <p role="status">Checking your sign-in methods…</p>
      {:else if identities.isError}
        <p class="field-error" role="alert">Your sign-in methods are unavailable. <button class="text-button" type="button" onclick={() => identities.refetch()}>Try again</button></p>
      {:else}
        {#if !passwordEnrollmentNeeded}<div class="form-field"><label for="current-password">Current password</label><input id="current-password" bind:value={currentPassword} type="password" autocomplete="current-password" aria-invalid={Boolean(passwordError)} aria-describedby={passwordError ? 'password-error' : undefined} /></div>{/if}
        <div class="form-field"><label for="new-password">New password</label><input id="new-password" bind:value={newPassword} type="password" autocomplete="new-password" aria-invalid={Boolean(passwordError)} aria-describedby={passwordError ? 'new-password-help password-error' : 'new-password-help'} /><small id="new-password-help">Use at least 12 characters.</small></div>
        <div class="form-field"><label for="confirm-password">Confirm new password</label><input id="confirm-password" bind:value={confirmPassword} type="password" autocomplete="new-password" aria-invalid={Boolean(passwordError)} aria-describedby={passwordError ? 'password-error' : undefined} /></div>
        {#if passwordError}<p id="password-error" class="field-error" role="alert">{passwordError}</p>{/if}
        <button class="button button-primary" type="submit" disabled={savingPassword}>{savingPassword ? 'Saving…' : passwordEnrollmentNeeded ? 'Add local password' : 'Change password'}</button>
        <p class="security-note">{passwordEnrollmentNeeded ? 'Add a password before unlinking your final OIDC identity.' : 'Changing your password revokes every other session.'}</p>
      {/if}
    </form>

    <section class="card panel oidc-panel" aria-labelledby="linked-identities-title">
      <div><p class="eyebrow">Federated authentication</p><h2 id="linked-identities-title">Linked OIDC identities</h2></div>
      {#if identities.isPending}<p role="status">Loading linked identities…</p>
      {:else if identities.isError}<p class="field-error" role="alert">Linked identities are unavailable. <button class="text-button" type="button" onclick={() => identities.refetch()}>Try again</button></p>
      {:else}
        {#if identities.data?.data.length}
          <div class="identity-list">{#each identities.data.data as identity (identity.id)}<article class="identity-row"><div><strong>{identity.email_at_link ?? 'OIDC identity'}</strong><small>{identity.issuer}</small><small>{identity.last_login_at ? `Last used ${formatDate(identity.last_login_at)}` : `Linked ${formatDate(identity.created_at)}`}</small></div><button class="button button-secondary" type="button" onclick={() => unlinkIdentity(identity.id, identity.issuer)} disabled={!identity.can_unlink || Boolean(identityBusy)} title={identity.can_unlink ? 'Unlink this identity' : 'Add another authentication method before unlinking'}>{identityBusy === identity.id ? 'Unlinking…' : 'Unlink'}</button></article>{/each}</div>
        {:else}<p class="security-note">No OIDC identity is linked to this account.</p>{/if}
        {#if identities.data?.linking_available}<button class="button button-secondary" type="button" onclick={linkIdentity} disabled={Boolean(identityBusy)}>{identityBusy === 'link' ? 'Redirecting…' : 'Link an OIDC identity'}</button>{/if}
      {/if}
      {#if identityError}<p class="field-error" role="alert">{identityError}</p>{/if}
      <p class="security-note">The final sign-in method cannot be removed, so OIDC-only accounts stay recoverable.</p>
    </section>
  </div>

  <section class="sessions" aria-labelledby="sessions-title"><div class="section-heading"><div><p class="eyebrow">Security</p><h2 id="sessions-title">Active sessions</h2><p>Review signed-in browsers and revoke anything you do not recognize.</p></div><button class="button button-secondary" type="button" onclick={() => sessions.refetch()} disabled={sessions.isFetching}>Refresh</button></div>
    {#if sessions.isPending}<div class="loading-state" role="status">Loading sessions…</div>
    {:else if sessions.isError}<div class="inline-problem" role="alert">Sessions are unavailable.</div>
    {:else}<div class="session-list">{#each sessions.data?.data ?? [] as session (session.id)}<article class="card session-row"><div class="session-icon" aria-hidden="true">{session.current ? '●' : '○'}</div><div><div class="session-heading"><strong>{session.current ? 'Current session' : 'Browser session'}</strong>{#if session.current}<span class="badge success">This device</span>{/if}</div><p>Last active {formatDate(session.last_seen_at)} · Expires {formatDate(session.expires_at)}</p><small class="mono">{session.id}</small></div><button class="button button-secondary" type="button" onclick={() => endSession(session.id, session.current)} disabled={revoking === session.id}>{revoking === session.id ? 'Revoking…' : session.current ? 'Sign out' : 'Revoke'}</button></article>{/each}</div>{#if sessionHistory.length > 0 || sessions.data?.next_cursor}<nav class="pagination" aria-label="Session pages"><button class="button button-secondary" type="button" onclick={previousSessions} disabled={sessionHistory.length === 0}>Previous</button><span>Page {sessionHistory.length + 1}</span><button class="button button-secondary" type="button" onclick={nextSessions} disabled={!sessions.data?.next_cursor}>Next</button></nav>{/if}{/if}
  </section>
{/if}

<style>
  .success-message { margin: 1rem 0 0; padding: 0.8rem 1rem; border-radius: 0.375rem; background: var(--success-soft); color: var(--success); font-weight: 700; }
  .profile-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1rem; margin-top: 1.5rem; align-items: start; }
  .panel { display: grid; gap: 1rem; padding: 1.25rem; }
  .oidc-panel { grid-column: 1 / -1; }
  .identity-list { display: grid; gap: .65rem; }
  .identity-row { display: flex; min-width: 0; align-items: center; justify-content: space-between; gap: 1rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; }
  .identity-row > div { display: grid; min-width: 0; gap: .15rem; }
  .identity-row small { overflow-wrap: anywhere; color: var(--foreground-muted); }
  h2 { margin: 0; font-size: 1.2rem; }
  .form-field input { width: 100%; }
  dl { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1rem; margin: 0; }
  dt { color: var(--foreground-muted); font-size: 0.7rem; font-weight: 700; }
  dd { margin: 0.1rem 0 0; }
  .field-error { margin: 0; color: var(--danger); font-weight: 700; }
  .security-note { margin: 0; color: var(--foreground-muted); font-size: 0.75rem; }
  .sessions { margin-top: 2rem; }
  .section-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; margin-bottom: 0.8rem; }
  .section-heading p:last-child { margin: 0.3rem 0 0; color: var(--foreground-muted); }
  .session-list { display: grid; gap: 0.7rem; }
  .session-row { display: grid; grid-template-columns: auto 1fr auto; align-items: center; gap: 0.85rem; padding: 1rem; }
  .session-icon { color: var(--success); }
  .session-heading { display: flex; align-items: center; gap: 0.5rem; }
  .session-row p, .session-row small { display: block; margin: 0.2rem 0 0; color: var(--foreground-muted); overflow-wrap: anywhere; }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  .pagination { display: flex; align-items: center; justify-content: flex-end; gap: 1rem; margin-top: 1rem; }
  .pagination span { color: var(--foreground-muted); }
  @media (max-width: 62rem) { .profile-grid { grid-template-columns: 1fr; } }
  @media (max-width: 40rem) { .panel { padding: 0.85rem; } .identity-row { display: grid; } .session-row { grid-template-columns: auto 1fr; } .session-row .button { grid-column: 1 / -1; } .section-heading { display: grid; } }
</style>
