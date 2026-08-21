<script lang="ts">
  import type { UserProfile } from '$lib/api/profile';

  type Props = {
    profile: UserProfile;
    displayName: string;
    displayNameError: string;
    profileError: string;
    saving: boolean;
    onDisplayName: (value: string) => void;
    onSave: (event: SubmitEvent) => void | Promise<void>;
  };

  let {
    profile,
    displayName,
    displayNameError,
    profileError,
    saving,
    onDisplayName,
    onSave
  }: Props = $props();
</script>

<form class="card panel" onsubmit={onSave}>
  <div>
    <p class="eyebrow">Identity</p>
    <h2>Profile details</h2>
  </div>
  <div class="form-field">
    <label for="profile-name">Display name</label>
    <input
      id="profile-name"
      name="display_name"
      value={displayName}
      oninput={(event) => onDisplayName(event.currentTarget.value)}
      autocomplete="name"
      aria-describedby={displayNameError
        ? 'profile-name-help profile-name-error'
        : 'profile-name-help'}
      aria-invalid={Boolean(displayNameError)}
    />
    <small id="profile-name-help"
      >Your email and fixed role are managed by an owner.</small
    >
    {#if displayNameError}<small id="profile-name-error" class="field-error"
        >{displayNameError}</small
      >{/if}
  </div>
  <dl>
    <div>
      <dt>Email</dt>
      <dd>{profile.email}</dd>
    </div>
    <div>
      <dt>Role</dt>
      <dd><span class="badge accent">{profile.role}</span></dd>
    </div>
  </dl>
  {#if profileError}<p id="profile-error" class="field-error" role="alert">
      {profileError}
    </p>{/if}
  <button class="button button-primary" type="submit" disabled={saving}
    >{saving ? 'Saving…' : 'Save profile'}</button
  >
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
  dl {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 1rem;
    margin: 0;
  }
  dt {
    color: var(--foreground-muted);
    font-size: 0.7rem;
    font-weight: 700;
  }
  dd {
    margin: 0.1rem 0 0;
  }
  .field-error {
    margin: 0;
    color: var(--danger);
    font-weight: 700;
  }
  @media (max-width: 40rem) {
    .panel {
      padding: 0.85rem;
    }
  }
</style>
