<script lang="ts">
  import { onDestroy } from 'svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { ApiProblem } from '#lib/management/http.js';
  import { createOwner } from '#lib/features/identity/setup/api.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import {
    PASSWORD_MAX_LENGTH,
    PASSWORD_MIN_LENGTH
  } from '#lib/passwordPolicy.js';
  import {
    type OwnerFormValues,
    ownerServerFields,
    validateOwner
  } from '#lib/features/identity/setup/ownerValidation.js';

  let { onComplete }: { onComplete: () => void } = $props();

  const values = $state<OwnerFormValues>({
    displayName: '',
    installationName: '',
    email: '',
    password: '',
    confirmPassword: '',
    setupToken: ''
  });
  const action = useAction<keyof OwnerFormValues>(undefined, 'authentication');
  const errors = $derived(action.fieldErrors);

  function describedBy(field: keyof OwnerFormValues, hint?: string) {
    const parts = [hint, errors[field] ? `${field}-error` : undefined].filter(
      Boolean
    );
    return parts.length ? parts.join(' ') : undefined;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    if (!action.check(validateOwner(values))) return focusFormError(form);

    const installationName = values.installationName.trim();
    const accepted = await action.run(
      () =>
        authLifecycle.authenticate((signal) =>
          createOwner(
            {
              display_name: values.displayName.trim(),
              // Omitted rather than sent blank: the API rejects an empty name and
              // applies its own default when the field is absent.
              ...(installationName
                ? { installation_name: installationName }
                : {}),
              email: values.email.trim(),
              password: values.password
            },
            values.setupToken,
            signal
          )
        ),
      () => onComplete(),
      {
        serverFields: ownerServerFields,
        failure: (cause) =>
          cause instanceof ApiProblem
            ? (cause.problem.detail ?? cause.problem.title)
            : 'The control API could not be reached. Check the service and try again.'
      }
    );
    if (!accepted) await focusFormError(form);
  }

  onDestroy(() => authLifecycle.abortAuthenticationWork());
</script>

<div class="heading">
  <p class="eyebrow">Installation owner</p>
  <h2>Create your account</h2>
  <p>
    This first account owns the installation. You can invite operators and
    developers next.
  </p>
</div>

<form novalidate onsubmit={submit}>
  {#if action.error}
    <div class="form-alert" role="alert" tabindex="-1" data-error-summary>
      <strong>Setup could not continue</strong>
      <span>{action.error}</span>
    </div>
  {/if}

  <div class="field">
    <label for="display-name">Display name</label>
    <input
      id="display-name"
      name="display_name"
      type="text"
      autocomplete="name"
      maxlength="100"
      bind:value={values.displayName}
      aria-invalid={errors.displayName ? 'true' : undefined}
      aria-describedby={describedBy('displayName')}
      disabled={action.busy}
    />
    {#if errors.displayName}<p class="field-error" id="displayName-error">
        {errors.displayName}
      </p>{/if}
  </div>

  <div class="field">
    <label for="installation-name">Installation name (optional)</label>
    <input
      id="installation-name"
      name="installation_name"
      type="text"
      autocomplete="organization"
      maxlength="100"
      bind:value={values.installationName}
      aria-invalid={errors.installationName ? 'true' : undefined}
      aria-describedby={describedBy(
        'installationName',
        'installation-name-hint'
      )}
      disabled={action.busy}
    />
    <p class="field-hint" id="installation-name-hint">
      Names this deployment in the console. Leave blank to keep the default.
    </p>
    {#if errors.installationName}<p
        class="field-error"
        id="installationName-error"
      >
        {errors.installationName}
      </p>{/if}
  </div>

  <div class="field">
    <label for="owner-email">Work email</label>
    <input
      id="owner-email"
      name="email"
      type="email"
      inputmode="email"
      autocomplete="username"
      autocapitalize="none"
      spellcheck="false"
      maxlength="254"
      bind:value={values.email}
      aria-invalid={errors.email ? 'true' : undefined}
      aria-describedby={describedBy('email')}
      disabled={action.busy}
    />
    {#if errors.email}<p class="field-error" id="email-error">
        {errors.email}
      </p>{/if}
  </div>

  <div class="field">
    <label for="owner-password">Password</label>
    <input
      id="owner-password"
      name="password"
      type="password"
      autocomplete="new-password"
      minlength={PASSWORD_MIN_LENGTH}
      maxlength={PASSWORD_MAX_LENGTH}
      bind:value={values.password}
      aria-invalid={errors.password ? 'true' : undefined}
      aria-describedby={describedBy('password', 'password-hint')}
      disabled={action.busy}
    />
    <p class="field-hint" id="password-hint">
      At least {PASSWORD_MIN_LENGTH} characters. A password manager is recommended.
    </p>
    {#if errors.password}<p class="field-error" id="password-error">
        {errors.password}
      </p>{/if}
  </div>

  <div class="field">
    <label for="confirm-password">Confirm password</label>
    <input
      id="confirm-password"
      name="confirm_password"
      type="password"
      autocomplete="new-password"
      bind:value={values.confirmPassword}
      aria-invalid={errors.confirmPassword ? 'true' : undefined}
      aria-describedby={describedBy('confirmPassword')}
      disabled={action.busy}
    />
    {#if errors.confirmPassword}<p
        class="field-error"
        id="confirmPassword-error"
      >
        {errors.confirmPassword}
      </p>{/if}
  </div>

  <div class="field">
    <label for="setup-token">Setup token</label>
    <input
      id="setup-token"
      name="setup_token"
      type="password"
      autocomplete="off"
      spellcheck="false"
      bind:value={values.setupToken}
      aria-invalid={errors.setupToken ? 'true' : undefined}
      aria-describedby={describedBy('setupToken', 'setup-token-hint')}
      disabled={action.busy}
    />
    <p class="field-hint" id="setup-token-hint">
      Print it on the server with <code>olp setup-token</code>. It is not saved
      by this console.
    </p>
    {#if errors.setupToken}<p class="field-error" id="setupToken-error">
        {errors.setupToken}
      </p>{/if}
  </div>

  <button
    class="button button-primary submit"
    type="submit"
    disabled={action.busy}
    aria-busy={action.busy}
  >
    {action.busy ? 'Creating owner…' : 'Create owner account'}
    {#if !action.busy}<NavIcon name="arrow" />{/if}
  </button>
</form>

<p class="privacy-note">
  Credentials are sent only to this installation and are never written to
  console storage.
</p>

<style>
  .heading {
    margin-bottom: 1.75rem;
  }

  h2 {
    margin: 0;
    font-size: 1.75rem;
    font-weight: 400;
    letter-spacing: -0.031em;
    line-height: 1.1;
  }

  .heading > p:last-child {
    margin: 0.75rem 0 0;
    color: var(--foreground-muted);
  }

  .form-alert {
    display: grid;
    gap: 0.15rem;
    padding: 0.75rem 0.85rem;
    border: 1px solid var(--danger);
    border-radius: var(--radius-control);
    background: var(--danger-soft);
    color: var(--foreground);
  }

  .form-alert strong {
    color: var(--danger);
  }

  form {
    display: grid;
    gap: 1rem;
  }

  .field {
    display: grid;
    gap: 0.4rem;
  }

  label {
    font-weight: 500;
  }

  input {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
  }

  input:hover:not(:disabled) {
    border-color: var(--border-strong);
  }

  input[aria-invalid='true'] {
    border-color: var(--danger);
  }

  .field-hint,
  .field-error {
    margin: 0;
    font-size: var(--text-caption);
  }

  .field-hint {
    color: var(--foreground-muted);
  }

  .submit {
    width: 100%;
    margin-top: 0.35rem;
  }

  .privacy-note {
    margin: 1.15rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
    text-align: center;
  }
</style>
