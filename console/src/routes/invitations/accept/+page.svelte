<script lang="ts">
  import { goto, replaceState } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { onMount } from 'svelte';
  import { acceptInvitation } from '$lib/api/auth';
  import { ApiProblem } from '$lib/api/http';
  import SetupFrame from '$lib/features/setup/SetupFrame.svelte';
  import {
    type InvitationAcceptanceErrors,
    type InvitationAcceptanceValues,
    validateInvitationAcceptance
  } from '$lib/features/access/invitationValidation';

  let token = $state('');
  let view = $state<'checking' | 'ready' | 'invalid' | 'expired'>('checking');
  let values = $state<InvitationAcceptanceValues>({
    displayName: '',
    password: '',
    confirmPassword: ''
  });
  let errors = $state<InvitationAcceptanceErrors>({});
  let message = $state('');
  let busy = $state(false);

  const serverFields: Record<string, keyof InvitationAcceptanceValues> = {
    display_name: 'displayName',
    password: 'password'
  };

  onMount(() => {
    const fragment = new URLSearchParams(window.location.hash.replace(/^#/, ''));
    token = fragment.get('token') ?? '';
    // Remove the one-time material before the invitee enters any data. The
    // token remains only in this component's memory until submission.
    view = token ? 'ready' : 'invalid';
    const scrub = window.setTimeout(() => replaceState(resolve('/invitations/accept'), {}), 0);
    return () => {
      window.clearTimeout(scrub);
      token = '';
      values.password = '';
      values.confirmPassword = '';
    };
  });

  function applyFieldErrors(problem: ApiProblem) {
    const next: InvitationAcceptanceErrors = {};
    for (const [field, fieldMessages] of Object.entries(problem.problem.errors ?? {})) {
      const local = serverFields[field];
      if (local && fieldMessages[0]) next[local] = fieldMessages[0];
    }
    errors = next;
    return Object.keys(next).length > 0;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    message = '';
    errors = validateInvitationAcceptance(values);
    if (Object.keys(errors).length || !token) return;
    busy = true;
    try {
      await acceptInvitation({
        token,
        display_name: values.displayName.trim(),
        password: values.password
      });
      token = '';
      values.password = '';
      values.confirmPassword = '';
      await goto(resolve('/'), { replaceState: true, invalidateAll: true });
    } catch (error) {
      if (error instanceof ApiProblem) {
        if (error.problem.status === 410) {
          token = '';
          view = 'expired';
        } else if (!applyFieldErrors(error)) {
          message = error.problem.detail ?? error.problem.title;
        }
      } else {
        message = 'The control API could not be reached. Check the installation and try again.';
      }
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head>
  <title>Accept invitation · OpenLLMProxy</title>
  <meta name="referrer" content="no-referrer" />
</svelte:head>

<SetupFrame>
  {#if view === 'checking'}
    <div class="state" role="status"><h1>Opening invitation…</h1></div>
  {:else if view === 'invalid'}
    <div class="state" role="alert">
      <p class="eyebrow">Invitation unavailable</p>
      <h1>This invitation link is incomplete.</h1>
      <p>Ask an installation owner for a fresh one-time invitation link.</p>
      <a class="button button-secondary" href="/login">Go to sign in</a>
    </div>
  {:else if view === 'expired'}
    <div class="state" role="alert">
      <p class="eyebrow">Invitation unavailable</p>
      <h1>This invitation can no longer be used.</h1>
      <p>It may be expired, revoked, or already accepted. Ask an installation owner for a new link.</p>
      <a class="button button-secondary" href="/login">Go to sign in</a>
    </div>
  {:else}
    <div class="heading">
      <p class="eyebrow">Team invitation</p>
      <h1>Finish creating your account.</h1>
      <p>Your email and fixed role were selected by the installation owner.</p>
    </div>

    {#if message}<div class="form-alert" role="alert">{message}</div>{/if}

    <form novalidate onsubmit={submit}>
      <label for="invited-display-name">Display name</label>
      <input id="invited-display-name" autocomplete="name" maxlength="100" bind:value={values.displayName} aria-invalid={errors.displayName ? 'true' : undefined} aria-describedby={errors.displayName ? 'display-name-error' : undefined} disabled={busy} />
      {#if errors.displayName}<small id="display-name-error" class="field-error">{errors.displayName}</small>{/if}

      <label for="invited-password">Password</label>
      <input id="invited-password" type="password" autocomplete="new-password" minlength="12" maxlength="1024" bind:value={values.password} aria-invalid={errors.password ? 'true' : undefined} aria-describedby={errors.password ? 'password-help password-error' : 'password-help'} disabled={busy} />
      <small id="password-help">Use at least 12 characters.</small>
      {#if errors.password}<small id="password-error" class="field-error">{errors.password}</small>{/if}

      <label for="invited-confirm-password">Confirm password</label>
      <input id="invited-confirm-password" type="password" autocomplete="new-password" bind:value={values.confirmPassword} aria-invalid={errors.confirmPassword ? 'true' : undefined} aria-describedby={errors.confirmPassword ? 'confirm-password-error' : undefined} disabled={busy} />
      {#if errors.confirmPassword}<small id="confirm-password-error" class="field-error">{errors.confirmPassword}</small>{/if}

      <button class="button button-primary" type="submit" disabled={busy} aria-busy={busy}>{busy ? 'Creating account…' : 'Accept invitation'}</button>
    </form>
  {/if}
</SetupFrame>

<style>
  .heading { margin-bottom: 1.5rem; }
  h1 { margin: 0; font-size: clamp(1.7rem, 4vw, 2.1rem); letter-spacing: -.03em; line-height: 1.12; }
  .heading > p:last-child, .state p, form small { color: var(--foreground-muted); }
  form { display: grid; gap: .45rem; }
  form label { margin-top: .55rem; font-weight: 700; }
  form input { min-height: 2.5rem; padding: .5rem .7rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  form .button { margin-top: 1rem; }
  .field-error { color: var(--danger); }
  .form-alert { margin-bottom: 1rem; padding: .7rem .8rem; border: 1px solid var(--danger); border-radius: .375rem; background: var(--danger-soft); color: var(--danger); }
  .state { display: grid; justify-items: start; gap: .8rem; }
  .state p { margin: 0; }
</style>
