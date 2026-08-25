<script lang="ts">
  import { onMount } from 'svelte';
  import { replaceState } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '$lib/api/http';
  import { beginOidcLink } from '$lib/api/management/oidc';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ReauthenticateDialog from '$lib/components/ReauthenticateDialog.svelte';
  import {
    acceptRemote,
    beginReload,
    conflictNotice,
    initialConcurrentEdit,
    markConflict,
    markDirty,
    markSaved,
    reconcile
  } from '$lib/forms/concurrentEdit';
  import {
    beginOidcReauthentication,
    changePassword,
    enrollPassword,
    getProfile,
    listOidcIdentities,
    reauthenticateWithPassword,
    unlinkOidcIdentity,
    updateProfile
  } from '$lib/api/profile';
  import ActiveSessionsPanel from './ActiveSessionsPanel.svelte';
  import LocalPasswordPanel from './LocalPasswordPanel.svelte';
  import OidcIdentitiesPanel from './OidcIdentitiesPanel.svelte';
  import ProfileDetailsPanel from './ProfileDetailsPanel.svelte';
  import {
    ENROLLMENT_GRANT_READY_MESSAGE,
    ENROLLMENT_GRANT_TTL_MS,
    parseRecentAuthenticationCallback,
    type PendingIdentityAction
  } from './recentAuthentication';
  import {
    validateDisplayName,
    validateNewPassword,
    validatePassword
  } from './validation';

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
  let identityBusy = $state('');
  let identityError = $state('');
  let pendingIdentityAction = $state<PendingIdentityAction | undefined>();
  type ReauthenticationRequest = {
    purpose: 'oidc_link' | 'oidc_unlink';
    resourceId?: string;
  };
  let reauthenticationRequest = $state<ReauthenticationRequest | null>(null);
  let reauthenticationBusy = $state(false);
  let reauthenticationError = $state('');
  let enrollmentGrantReady = $state(false);
  let enrollmentGrantExpiry: ReturnType<typeof setTimeout> | undefined;
  let profileSync = $state(initialConcurrentEdit());
  const profileConcurrentNotice = $derived(conflictNotice(profileSync));
  const queryClient = useQueryClient();

  const profile = createQuery(() => ({
    queryKey: ['profile'],
    queryFn: getProfile
  }));
  const identities = createQuery(() => ({
    queryKey: ['profile-oidc-identities'],
    queryFn: listOidcIdentities
  }));
  let passwordEnrollmentNeeded = $derived(
    identities.data ? !identities.data.has_local_password : false
  );

  $effect(() => {
    const value = profile.data;
    if (!value) return;
    const next = reconcile(profileSync, value.etag);
    if (next.state !== profileSync) profileSync = next.state;
    if (!next.hydrate) return;
    displayName = value.display_name;
    displayNameError = '';
  });

  onMount(() => {
    const callback = parseRecentAuthenticationCallback(window.location.search);
    if (callback) {
      replaceState(resolve('/settings/profile'), {});
      // Callback query parameters are only a display hint. They must never
      // directly begin a link or unlink request, because another site can
      // navigate a signed-in browser to this URL.
      void resumeSecurityOperation(callback.purpose, callback.resourceId);
    }
    return cancelEnrollmentGrantExpiry;
  });

  function cancelEnrollmentGrantExpiry() {
    if (enrollmentGrantExpiry === undefined) return;
    clearTimeout(enrollmentGrantExpiry);
    enrollmentGrantExpiry = undefined;
  }

  function clearEnrollmentGrant() {
    cancelEnrollmentGrantExpiry();
    enrollmentGrantReady = false;
  }

  function markEnrollmentGrantReady() {
    clearEnrollmentGrant();
    enrollmentGrantReady = true;
    enrollmentGrantExpiry = setTimeout(() => {
      enrollmentGrantExpiry = undefined;
      enrollmentGrantReady = false;
      if (message === ENROLLMENT_GRANT_READY_MESSAGE) message = '';
      passwordError =
        'Identity verification expired. Verify your identity with OIDC again.';
    }, ENROLLMENT_GRANT_TTL_MS);
  }

  async function resumeSecurityOperation(
    purpose: 'password_enrollment' | 'oidc_link' | 'oidc_unlink',
    resourceId?: string
  ) {
    identityError = passwordError = message = '';
    try {
      if (purpose === 'password_enrollment') {
        pendingIdentityAction = undefined;
        markEnrollmentGrantReady();
        message = ENROLLMENT_GRANT_READY_MESSAGE;
        return;
      }
      if (purpose === 'oidc_link') {
        pendingIdentityAction = { purpose };
        message =
          'Identity verified. Confirm before linking your OIDC identity.';
        return;
      }
      if (!resourceId)
        throw new Error('The identity selected for unlinking is missing.');
      pendingIdentityAction = { purpose, resourceId };
      message =
        'Identity verified. Confirm before unlinking this OIDC identity.';
    } catch (cause) {
      identityError =
        cause instanceof Error
          ? cause.message
          : 'The security operation could not be completed.';
    } finally {
      identityBusy = '';
    }
  }

  async function completePendingIdentityAction() {
    const action = pendingIdentityAction;
    if (!action) return;
    identityBusy = action.purpose === 'oidc_link' ? 'link' : action.resourceId;
    identityError = message = '';
    try {
      if (action.purpose === 'oidc_link') {
        window.location.assign(await beginOidcLink());
        return;
      }
      await unlinkOidcIdentity(action.resourceId);
      pendingIdentityAction = undefined;
      message =
        'OIDC identity unlinked. All previous sessions were revoked and this browser was rotated.';
      await refreshSecurityData();
    } catch (cause) {
      identityError =
        cause instanceof Error
          ? cause.message
          : 'The security operation could not be completed.';
    } finally {
      identityBusy = '';
    }
  }

  /**
   * Recent authentication is proven either by an inline password confirmation
   * or by an OIDC round trip. The password path opens a real form so the value
   * is masked, cancellable, and never typed into a browser prompt.
   */
  async function acquireRecentAuthentication(
    purpose: 'oidc_link' | 'oidc_unlink',
    resourceId?: string
  ): Promise<void> {
    if (identities.data?.has_local_password) {
      reauthenticationError = '';
      reauthenticationRequest = { purpose, resourceId };
      return;
    }
    window.location.assign(
      await beginOidcReauthentication(purpose, resourceId)
    );
  }

  function cancelReauthentication() {
    reauthenticationRequest = null;
    reauthenticationError = '';
  }

  async function confirmReauthentication(password: string) {
    const request = reauthenticationRequest;
    if (!request) return;
    reauthenticationBusy = true;
    reauthenticationError = '';
    try {
      await reauthenticateWithPassword(
        password,
        request.purpose,
        request.resourceId
      );
      reauthenticationRequest = null;
      identityBusy =
        request.purpose === 'oidc_link' ? 'link' : (request.resourceId ?? '');
      if (request.purpose === 'oidc_link') {
        window.location.assign(await beginOidcLink());
        return;
      }
      if (!request.resourceId)
        throw new Error('The identity selected for unlinking is missing.');
      await unlinkOidcIdentity(request.resourceId);
      message =
        'OIDC identity unlinked. All previous sessions were revoked and this browser was rotated.';
      await refreshSecurityData();
    } catch (cause) {
      const detail = errorMessage(
        cause,
        'The security operation could not be completed.'
      );
      if (reauthenticationRequest) reauthenticationError = detail;
      else identityError = detail;
    } finally {
      reauthenticationBusy = false;
      identityBusy = '';
    }
  }

  async function verifyPasswordEnrollment() {
    passwordError = message = '';
    savingPassword = true;
    try {
      window.location.assign(
        await beginOidcReauthentication('password_enrollment')
      );
    } catch (cause) {
      passwordError =
        cause instanceof Error
          ? cause.message
          : 'Identity verification could not start.';
      savingPassword = false;
    }
  }

  function changeDisplayName(value: string) {
    displayName = value;
    profileSync = markDirty(profileSync);
    try {
      validateDisplayName(value);
      displayNameError = '';
    } catch (cause) {
      displayNameError =
        cause instanceof Error ? cause.message : 'Enter a valid display name.';
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
      displayNameError =
        cause instanceof Error ? cause.message : 'Enter a valid display name.';
      return;
    }
    profileError = message = '';
    savingProfile = true;
    try {
      if (!profileSync.snapshotEtag)
        throw new Error('Reload your profile before saving.');
      const updated = await updateProfile(
        { ...profile.data, etag: profileSync.snapshotEtag },
        { display_name: normalizedDisplayName }
      );
      profileSync = markSaved(profileSync, updated.etag);
      queryClient.setQueryData(['profile'], updated);
      message = 'Profile updated.';
    } catch (cause) {
      if (isEtagMismatch(cause)) profileSync = markConflict(profileSync);
      else
        profileError =
          cause instanceof Error
            ? cause.message
            : 'The profile could not be updated.';
    } finally {
      savingProfile = false;
    }
  }

  async function savePassword(event: SubmitEvent) {
    event.preventDefault();
    if (!profile.data) return;
    passwordError = message = '';
    savingPassword = true;
    if (passwordEnrollmentNeeded && !enrollmentGrantReady) {
      passwordError =
        'Verify your identity with OIDC before adding a local password.';
      savingPassword = false;
      return;
    }
    let enrollmentSubmitted = false;
    try {
      if (!profileSync.snapshotEtag)
        throw new Error('Reload your profile before saving.');
      const snapshot = { ...profile.data, etag: profileSync.snapshotEtag };
      const next = passwordEnrollmentNeeded
        ? validateNewPassword(newPassword, confirmPassword)
        : validatePassword(currentPassword, newPassword, confirmPassword);
      let updated;
      if (passwordEnrollmentNeeded) {
        enrollmentSubmitted = true;
        updated = await enrollPassword(snapshot, { new_password: next });
      } else {
        updated = await changePassword(snapshot, {
          current_password: currentPassword,
          new_password: next
        });
      }
      profileSync = acceptRemote(profileSync, updated.etag);
      queryClient.setQueryData(['profile'], updated);
      currentPassword = newPassword = confirmPassword = '';
      clearEnrollmentGrant();
      message = passwordEnrollmentNeeded
        ? 'Local password added. All previous sessions were revoked and this browser was rotated.'
        : 'Password changed. All previous sessions were revoked and this browser was rotated.';
      await refreshSecurityData();
    } catch (cause) {
      if (enrollmentSubmitted) clearEnrollmentGrant();
      if (isEtagMismatch(cause)) profileSync = markConflict(profileSync);
      else {
        passwordError =
          cause instanceof Error
            ? cause.message
            : passwordEnrollmentNeeded
              ? 'The local password could not be added.'
              : 'The password could not be changed.';
      }
    } finally {
      savingPassword = false;
    }
  }

  async function linkIdentity() {
    pendingIdentityAction = undefined;
    identityBusy = 'link';
    identityError = '';
    try {
      await acquireRecentAuthentication('oidc_link');
    } catch (cause) {
      identityError =
        cause instanceof Error
          ? cause.message
          : 'The OIDC link flow could not start.';
    } finally {
      identityBusy = '';
    }
  }

  async function unlinkIdentity(id: string, issuer: string) {
    if (!confirm(`Unlink the identity from ${issuer}?`)) return;
    pendingIdentityAction = undefined;
    identityBusy = id;
    identityError = '';
    try {
      await acquireRecentAuthentication('oidc_unlink', id);
    } catch (cause) {
      identityError =
        cause instanceof Error
          ? cause.message
          : 'The OIDC identity could not be unlinked.';
    } finally {
      identityBusy = '';
    }
  }

  async function refetchProfile() {
    const result = await profile.refetch();
    if (result.data) profileSync = acceptRemote(profileSync, result.data.etag);
  }

  async function refreshSecurityData() {
    await Promise.all([
      refetchProfile(),
      identities.refetch(),
      queryClient.invalidateQueries({ queryKey: ['profile-sessions'] })
    ]);
  }

  async function reloadProfile() {
    const result = await profile.refetch();
    if (result.error) return;
    profileSync = beginReload(profileSync);
  }
</script>

<svelte:head><title>Personal profile · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Account</p>
    <h1 class="page-title">Personal profile</h1>
    <p class="page-description">
      Manage your display name, local password, and signed-in browser sessions.
    </p>
  </div>
  <a class="button button-secondary" href={resolve('/settings')}
    >Installation settings</a
  >
</div>

{#if reauthenticationRequest}
  <ReauthenticateDialog
    description={reauthenticationRequest.purpose === 'oidc_link'
      ? 'Linking an OIDC identity changes how you sign in, so confirm your current password first.'
      : 'Unlinking an OIDC identity revokes every other session, so confirm your current password first.'}
    busy={reauthenticationBusy}
    error={reauthenticationError}
    onConfirm={confirmReauthentication}
    onCancel={cancelReauthentication}
  />
{/if}

{#if message}<p class="success-message" role="status">{message}</p>{/if}
<ConflictNotice
  notice={profileConcurrentNotice}
  onReload={reloadProfile}
  disabled={savingProfile}
/>

{#if profile.isPending}<div class="loading-state" role="status">
    Loading your profile…
  </div>
{:else if profile.isError}<div class="inline-problem" role="alert">
    {errorMessage(profile.error, 'Your profile is unavailable.')}
    <button class="text-button" onclick={() => profile.refetch()}
      >Try again</button
    >
  </div>
{:else if profile.data}
  <div class="profile-grid">
    <ProfileDetailsPanel
      profile={profile.data}
      {displayName}
      {displayNameError}
      {profileError}
      saving={savingProfile}
      onDisplayName={changeDisplayName}
      onSave={saveProfile}
    />
    <LocalPasswordPanel
      identitiesPending={identities.isPending}
      identitiesError={identities.isError}
      enrollmentNeeded={passwordEnrollmentNeeded}
      {enrollmentGrantReady}
      bind:currentPassword
      bind:newPassword
      bind:confirmPassword
      {passwordError}
      saving={savingPassword}
      onRetryIdentities={() => identities.refetch()}
      onVerifyEnrollment={verifyPasswordEnrollment}
      onSave={savePassword}
    />
    <OidcIdentitiesPanel
      identities={identities.data}
      pending={identities.isPending}
      failed={identities.isError}
      pendingAction={pendingIdentityAction}
      busy={identityBusy}
      error={identityError}
      onRetry={() => identities.refetch()}
      onCompletePending={completePendingIdentityAction}
      onLink={linkIdentity}
      onUnlink={unlinkIdentity}
    />
  </div>
  <ActiveSessionsPanel />
{/if}

<style>
  .success-message {
    margin: 1rem 0 0;
    padding: 0.8rem 1rem;
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
    font-weight: 700;
  }
  .profile-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 1rem;
    margin-top: 1.5rem;
    align-items: start;
  }
  .text-button {
    min-height: 2.75rem;
    border: 0;
    background: transparent;
    color: var(--accent-strong);
    font-weight: 700;
  }
  @media (max-width: 62rem) {
    .profile-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
