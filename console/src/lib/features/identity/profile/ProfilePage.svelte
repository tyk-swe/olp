<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import SAMLIdentitiesPanel from '../saml/SAMLIdentitiesPanel.svelte';
  import { listSAMLIdentities } from '../saml/api';
  import { oidcFailureMessage } from '#lib/features/identity/login/oidcFailure.js';
  import { profileKeys } from '#lib/features/identity/profile/profileKeys.js';
  import { userKeys } from '#lib/features/identity/users/userKeys.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';

  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import ReauthenticateDialog from '#lib/features/identity/profile/ReauthenticateDialog.svelte';
  import {
    acceptRemote,
    beginReload,
    conflictNotice,
    initialConcurrentEdit,
    markConflict,
    markDirty,
    markSaved,
    reconcile
  } from '#lib/editing/concurrentEdit.js';
  import {
    beginIdentityReauthentication,
    changePassword,
    enrollPassword,
    getProfile,
    listOidcIdentities,
    updateProfile
  } from '#lib/features/identity/profile/api.js';
  import { captureResult } from '#lib/editing/result.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { useProfileIdentity } from './profileIdentity.svelte.js';
  import ActiveSessionsPanel from '#lib/features/identity/profile/ActiveSessionsPanel.svelte';
  import LocalPasswordPanel from '#lib/features/identity/profile/LocalPasswordPanel.svelte';
  import OidcIdentitiesPanel from '#lib/features/identity/profile/OidcIdentitiesPanel.svelte';
  import MFAPanel from '#lib/features/identity/mfa/MFAPanel.svelte';
  import ProfileDetailsPanel from '#lib/features/identity/profile/ProfileDetailsPanel.svelte';
  import {
    ENROLLMENT_VERIFIED_MESSAGE,
    ENROLLMENT_VERIFICATION_TTL_MS,
    parseRecentAuthenticationCallback
  } from '#lib/features/identity/profile/recentAuthentication.js';

  import {
    validateDisplayName,
    validateNewPassword,
    validatePassword
  } from '#lib/features/identity/profile/validation.js';

  const profileSave = useAction();
  const passwords = useAction();
  let displayName = $state('');
  let displayNameError = $state('');
  let currentPassword = $state('');
  let newPassword = $state('');
  let confirmPassword = $state('');
  let message = $state('');
  let enrollmentVerified = $state(false);
  let enrollmentVerificationExpiry: ReturnType<typeof setTimeout> | undefined;
  let profileEditVersion = 0;
  let profileSync = $state(initialConcurrentEdit());
  const profileConcurrentNotice = $derived(conflictNotice(profileSync));
  const queryClient = useQueryClient();

  const profile = createQuery(() => ({
    queryKey: profileKeys.current(),
    queryFn: getProfile
  }));
  const identities = createQuery(() => ({
    queryKey: profileKeys.oidcIdentities(),
    queryFn: listOidcIdentities
  }));
  const samlIdentities = createQuery(() => ({
    queryKey: profileKeys.saml(),
    queryFn: ({ signal }) => listSAMLIdentities(signal)
  }));
  let passwordEnrollmentNeeded = $derived(
    identities.data ? !identities.data.has_local_password : false
  );
  const identity = useProfileIdentity({
    identities: () => identities.data,
    refreshSecurityData,
    markEnrollmentVerified,
    clearMessages: () => {
      passwords.error = '';
      message = '';
    },
    setMessage: (value) => {
      message = value;
    }
  });

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
    identity.identity.error = oidcFailureMessage(
      new URLSearchParams(window.location.search).get('oidc_error')
    );
    const callback = parseRecentAuthenticationCallback(window.location.search);
    if (callback) {
      goto(resolve('settings/profile'), { shallow: true, replace: true });
      // Callback query parameters are only a display hint. They must never
      // directly begin a link or unlink request, because another site can
      // navigate a signed-in browser to this URL.
      identity.resumeSecurityOperation(callback.purpose, callback.resourceId);
    }
    return cancelEnrollmentVerificationExpiry;
  });

  function cancelEnrollmentVerificationExpiry() {
    if (enrollmentVerificationExpiry === undefined) return;
    clearTimeout(enrollmentVerificationExpiry);
    enrollmentVerificationExpiry = undefined;
  }

  function clearEnrollmentVerification() {
    cancelEnrollmentVerificationExpiry();
    enrollmentVerified = false;
  }

  function markEnrollmentVerified() {
    clearEnrollmentVerification();
    enrollmentVerified = true;
    enrollmentVerificationExpiry = setTimeout(() => {
      enrollmentVerificationExpiry = undefined;
      enrollmentVerified = false;
      if (message === ENROLLMENT_VERIFIED_MESSAGE) message = '';
      passwords.error =
        'Identity verification expired. Verify your linked identity again.';
    }, ENROLLMENT_VERIFICATION_TTL_MS);
  }

  function changeDisplayName(value: string) {
    displayName = value;
    profileEditVersion += 1;
    profileSync = markDirty(profileSync);
    const checked = captureResult(() => validateDisplayName(value));
    displayNameError = checked.ok
      ? ''
      : errorMessage(checked.error, 'Enter a valid display name.');
  }

  async function saveProfile(event: SubmitEvent) {
    event.preventDefault();
    if (!profile.data || profileSave.busy) return;
    const checked = captureResult(() => validateDisplayName(displayName));
    if (!checked.ok) {
      displayNameError = errorMessage(
        checked.error,
        'Enter a valid display name.'
      );
      return;
    }
    displayNameError = '';
    message = '';
    const submittedVersion = profileEditVersion;
    await profileSave.run(
      () => {
        if (!profileSync.snapshotEtag)
          throw new Error('Reload your profile before saving.');
        return updateProfile(
          { ...profile.data, etag: profileSync.snapshotEtag },
          { display_name: checked.value }
        );
      },
      async (updated) => {
        const hasInterveningEdits = profileEditVersion !== submittedVersion;
        profileSync = markSaved(updated.etag, hasInterveningEdits);
        if (!hasInterveningEdits) displayName = updated.display_name;
        queryClient.setQueryData(profileKeys.current(), updated);
        await queryClient.invalidateQueries({ queryKey: userKeys.root });
        message = hasInterveningEdits
          ? 'Profile updated. You have additional unsaved changes.'
          : 'Profile updated.';
        await authLifecycle.validateSession();
      },
      {
        failure: (cause) => {
          if (isEtagMismatch(cause)) {
            profileSync = markConflict(profileSync);
            return '';
          }
          return errorMessage(cause, 'The profile could not be updated.');
        }
      }
    );
  }

  async function savePassword(event: SubmitEvent) {
    event.preventDefault();
    if (!profile.data) return;
    message = '';
    if (passwordEnrollmentNeeded && !enrollmentVerified) {
      passwords.error =
        'Verify your linked identity before adding a local password.';
      return;
    }
    let enrollmentSubmitted = false;
    await passwords.run(
      () => {
        if (!profileSync.snapshotEtag)
          throw new Error('Reload your profile before saving.');
        const snapshot = { ...profile.data, etag: profileSync.snapshotEtag };
        const next = passwordEnrollmentNeeded
          ? validateNewPassword(newPassword, confirmPassword)
          : validatePassword(currentPassword, newPassword, confirmPassword);
        if (passwordEnrollmentNeeded) {
          enrollmentSubmitted = true;
          return enrollPassword(snapshot, { new_password: next });
        }
        return changePassword(snapshot, {
          current_password: currentPassword,
          new_password: next
        });
      },
      async (updated) => {
        profileSync = acceptRemote(profileSync, updated.etag);
        queryClient.setQueryData(profileKeys.current(), updated);
        currentPassword = newPassword = confirmPassword = '';
        clearEnrollmentVerification();
        message = passwordEnrollmentNeeded
          ? 'Local password added. All previous sessions were revoked and this browser was rotated.'
          : 'Password changed. All previous sessions were revoked and this browser was rotated.';
        await refreshSecurityData();
      },
      {
        failure: (cause) => {
          if (enrollmentSubmitted) clearEnrollmentVerification();
          if (isEtagMismatch(cause)) {
            profileSync = markConflict(profileSync);
            return '';
          }
          return errorMessage(
            cause,
            passwordEnrollmentNeeded
              ? 'The local password could not be added.'
              : 'The password could not be changed.'
          );
        }
      }
    );
  }

  async function unlinkIdentity(id: string, issuer: string) {
    if (
      !(await confirmAction({
        title: `Unlink ${issuer}?`,
        body: 'You won’t be able to sign in with this identity until you link it again.',
        confirmLabel: 'Unlink',
        danger: true
      }))
    )
      return;
    await identity.unlinkIdentity(id);
  }

  async function verifyPasswordEnrollment() {
    message = '';
    await passwords.run(
      () => beginIdentityReauthentication('password_enrollment'),
      (url) => window.location.assign(url),
      {
        failure: (cause) =>
          errorMessage(cause, 'Identity verification could not start.')
      }
    );
  }

  async function refetchProfile() {
    const result = await profile.refetch();
    if (result.data) profileSync = acceptRemote(profileSync, result.data.etag);
  }

  async function refreshSecurityData() {
    await Promise.all([
      refetchProfile(),
      identities.refetch(),
      queryClient.invalidateQueries({
        queryKey: profileKeys.sessionsRoot
      })
    ]);
  }

  async function reloadProfile() {
    const result = await profile.refetch();
    if (result.error) return;
    profileSync = beginReload(profileSync);
  }
</script>

<PageTitle title="Your profile" />

<PageHeader
  title="Your profile"
  description="Your name, password, linked sign-ins, and where you’re signed in."
/>
{#if identity.reauthenticationRequest}
  <ReauthenticateDialog
    title={identity.reauthenticationRequest.purpose === 'oidc_link'
      ? 'Confirm the OIDC link'
      : 'Confirm the OIDC unlink'}
    description={identity.reauthenticationRequest.purpose === 'oidc_link'
      ? 'Linking an OIDC identity changes how you sign in, so verify with your password or a usable linked identity.'
      : 'Unlinking an OIDC identity revokes every other session, so verify with your password or a usable linked identity.'}
    busy={identity.reauth.busy}
    error={identity.reauth.error}
    onOidc={identities.data?.oidc_reauthentication_available ||
    samlIdentities.data?.reauthentication_available
      ? identity.confirmWithOidc
      : undefined}
    onConfirm={identity.confirmReauthentication}
    onCancel={identity.cancelReauthentication}
  />
{/if}

{#if message}<Callout tone="success">{message}</Callout>{/if}
<ConflictNotice
  notice={profileConcurrentNotice}
  onReload={reloadProfile}
  disabled={profileSave.busy}
/>

<QueryView
  query={profile}
  loading="Loading your profile…"
  failure={errorMessage(profile.error, 'Your profile is unavailable.')}
  retryLabel="Try again"
  emptyWhen={!profile.data}
>
  {#snippet empty()}{/snippet}
  {#if profile.data}<div class="profile-grid">
      <ProfileDetailsPanel
        profile={profile.data}
        {displayName}
        {displayNameError}
        profileError={profileSave.error}
        saving={profileSave.busy}
        onDisplayName={changeDisplayName}
        onSave={saveProfile}
      />
      <SAMLIdentitiesPanel
        hasLocalPassword={identities.data?.has_local_password ?? false}
      />
      <MFAPanel
        hasLocalPassword={identities.data?.has_local_password ?? false}
        hasLinkedIdentity={(identities.data?.items.length ?? 0) > 0 ||
          Boolean(samlIdentities.data?.reauthentication_available)}
      />
      <LocalPasswordPanel
        identitiesPending={identities.isPending}
        identitiesError={identities.isError}
        enrollmentNeeded={passwordEnrollmentNeeded}
        {enrollmentVerified}
        bind:currentPassword
        bind:newPassword
        bind:confirmPassword
        passwordError={passwords.error}
        saving={passwords.busy}
        onRetryIdentities={() => identities.refetch()}
        onVerifyEnrollment={verifyPasswordEnrollment}
        onSave={savePassword}
      />
      <OidcIdentitiesPanel
        identities={identities.data}
        pending={identities.isPending}
        failed={identities.isError}
        pendingAction={identity.pendingIdentityAction}
        busy={identity.identity.key}
        error={identity.identity.error}
        onRetry={() => identities.refetch()}
        onCompletePending={identity.completePendingIdentityAction}
        onLink={identity.linkIdentity}
        onUnlink={unlinkIdentity}
      />
    </div>
    <ActiveSessionsPanel />{/if}
</QueryView>

<style>
  .profile-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 1rem;
    margin-top: 1.5rem;
    align-items: start;
  }
  @media (max-width: 62rem) {
    .profile-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
