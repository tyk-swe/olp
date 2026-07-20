<script lang="ts">
  import { ApiProblem } from '$lib/api/http';
  import { createOwner } from '$lib/api/setup';
  import { setCsrfToken } from '$lib/api/session';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import {
    type OwnerFormErrors,
    type OwnerFormValues,
    validateOwner
  } from './ownerValidation';

  let { onComplete }: { onComplete: () => void } = $props();

  let values = $state<OwnerFormValues>({
    displayName: '',
    email: '',
    password: '',
    confirmPassword: '',
    setupToken: ''
  });
  let errors = $state<OwnerFormErrors>({});
  let submissionError = $state('');
  let submitting = $state(false);

  const serverToFormField: Record<string, keyof OwnerFormValues> = {
    display_name: 'displayName',
    email: 'email',
    password: 'password',
    setup_token: 'setupToken'
  };

  function describedBy(field: keyof OwnerFormValues, hint?: string) {
    const parts = [hint, errors[field] ? `${field}-error` : undefined].filter(Boolean);
    return parts.length ? parts.join(' ') : undefined;
  }

  function applyServerErrors(problem: ApiProblem) {
    if (!problem.problem.errors) return false;
    const next: OwnerFormErrors = {};
    for (const [serverField, messages] of Object.entries(problem.problem.errors)) {
      const formField = serverToFormField[serverField];
      if (formField && messages[0]) next[formField] = messages[0];
    }
    errors = next;
    return Object.keys(next).length > 0;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    submissionError = '';
    errors = validateOwner(values);
    if (Object.keys(errors).length) return;

    submitting = true;
    try {
      const result = await createOwner(
        {
          display_name: values.displayName.trim(),
          email: values.email.trim(),
          password: values.password
        },
        values.setupToken
      );
      setCsrfToken(result.csrf_token);
      onComplete();
    } catch (error) {
      if (error instanceof ApiProblem) {
        if (!applyServerErrors(error)) submissionError = error.problem.detail ?? error.problem.title;
      } else {
        submissionError = 'The control API could not be reached. Check the service and try again.';
      }
    } finally {
      submitting = false;
    }
  }
</script>

<div class="step-count" aria-label="Setup step 1 of 6">
  <span>Step 1 of 6</span>
  <span class="progress" aria-hidden="true"><span></span></span>
</div>

<div class="heading">
  <p class="eyebrow">Installation owner</p>
  <h2>Create your account</h2>
  <p>This first account owns the installation. You can invite operators and developers next.</p>
</div>

{#if submissionError}
  <div class="form-alert" role="alert" tabindex="-1">
    <strong>Setup could not continue</strong>
    <span>{submissionError}</span>
  </div>
{/if}

<form novalidate onsubmit={submit}>
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
      disabled={submitting}
    />
    {#if errors.displayName}<p class="field-error" id="displayName-error">{errors.displayName}</p>{/if}
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
      disabled={submitting}
    />
    {#if errors.email}<p class="field-error" id="email-error">{errors.email}</p>{/if}
  </div>

  <div class="field">
    <label for="owner-password">Password</label>
    <input
      id="owner-password"
      name="password"
      type="password"
      autocomplete="new-password"
      minlength="12"
      maxlength="1024"
      bind:value={values.password}
      aria-invalid={errors.password ? 'true' : undefined}
      aria-describedby={describedBy('password', 'password-hint')}
      disabled={submitting}
    />
    <p class="field-hint" id="password-hint">At least 12 characters. A password manager is recommended.</p>
    {#if errors.password}<p class="field-error" id="password-error">{errors.password}</p>{/if}
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
      disabled={submitting}
    />
    {#if errors.confirmPassword}<p class="field-error" id="confirmPassword-error">{errors.confirmPassword}</p>{/if}
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
      disabled={submitting}
    />
    <p class="field-hint" id="setup-token-hint">Paste the one-time token supplied by your operator. It is not saved by this console.</p>
    {#if errors.setupToken}<p class="field-error" id="setupToken-error">{errors.setupToken}</p>{/if}
  </div>

  <button class="button button-primary submit" type="submit" disabled={submitting} aria-busy={submitting}>
    {submitting ? 'Creating owner…' : 'Create owner account'}
    {#if !submitting}<NavIcon name="arrow" />{/if}
  </button>
</form>

<p class="privacy-note">Credentials are sent only to this installation and are never written to console storage.</p>

<style>
  .step-count {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin-bottom: 2.2rem;
    color: var(--foreground-muted);
    font-size: 0.75rem;
    font-weight: 700;
  }

  .progress {
    display: block;
    width: 4.5rem;
    height: 0.25rem;
    overflow: hidden;
    border-radius: 999px;
    background: var(--surface-subtle);
  }

  .progress span {
    display: block;
    width: 16.667%;
    height: 100%;
    border-radius: inherit;
    background: var(--accent);
  }

  .heading {
    margin-bottom: 1.75rem;
  }

  h2 {
    margin: 0;
    font-size: clamp(1.75rem, 4vw, 2.25rem);
    font-weight: 730;
    letter-spacing: -0.04em;
    line-height: 1.12;
  }

  .heading > p:last-child {
    margin: 0.75rem 0 0;
    color: var(--foreground-muted);
  }

  .form-alert {
    display: grid;
    gap: 0.15rem;
    margin-bottom: 1rem;
    padding: 0.75rem 0.85rem;
    border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--border));
    border-radius: 0.375rem;
    background: var(--danger-soft);
    font-size: 0.8125rem;
  }

  form {
    display: grid;
    gap: 1rem;
  }

  .field {
    display: grid;
    gap: 0.35rem;
  }

  label {
    font-size: 0.8125rem;
    font-weight: 700;
  }

  input {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.7rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
    box-shadow: inset 0 1px 1px rgb(13 31 58 / 4%);
  }

  input:hover:not(:disabled) {
    border-color: var(--foreground-subtle);
  }

  input[aria-invalid='true'] {
    border-color: var(--danger);
  }

  .field-hint,
  .field-error {
    margin: 0;
    font-size: 0.75rem;
  }

  .field-hint {
    color: var(--foreground-muted);
  }

  .field-error {
    color: var(--danger);
    font-weight: 650;
  }

  .submit {
    width: 100%;
    margin-top: 0.35rem;
  }

  .privacy-note {
    margin: 1.15rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.72rem;
    text-align: center;
  }
</style>
