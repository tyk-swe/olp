<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { useBranding } from '#lib/shell/branding/branding.js';
  import { beginSAMLLogin } from '../saml/api';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { onDestroy, onMount } from 'svelte';
  import {
    authenticationCapabilities,
    beginOidcLogin,
    type AuthenticationCapabilities
  } from '#lib/session/api.js';
  import { login as passwordLogin } from '#lib/session/authentication.js';
  import { oidcFailureMessage } from '#lib/features/identity/login/oidcFailure.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { errorMessage } from '#lib/management/http.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import { relativeReturnTo } from '#lib/session/relativeReturnTo.js';
  import SetupFrame from '#lib/shell/SetupFrame.svelte';

  let email = $state('');
  let password = $state('');
  const login = useAction(undefined, 'authentication');
  const busy = $derived(login.key === 'password');
  const ssoBusy = $derived(login.key === 'sso');
  let capabilitiesLoading = $state(true);
  let capabilitiesError = $state('');
  const oidcError = $derived(
    oidcFailureMessage(page.url.searchParams.get('oidc_error'))
  );
  let capabilities = $state<AuthenticationCapabilities>({
    local_login_enabled: false,
    notifications_active: false,
    payload_capture_active: false,
    oidc_login_enabled: false
  });
  const publicAuthController = new AbortController();
  const sessionExpired = $derived(
    page.url.searchParams.get('reason') === 'expired'
  );

  function destination() {
    return relativeReturnTo(
      page.url.searchParams.get('return_to'),
      page.url.origin
    );
  }

  function oidcHref() {
    return `/api/v1/oidc/login?return_to=${encodeURIComponent(destination())}`;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (login.busy) return;
    login.error = '';
    if (!capabilities.local_login_enabled) {
      login.error =
        'Password-based sign-in is not available for this installation.';
      return;
    }
    if (!email.trim() || !password) {
      login.error = 'Enter your email and password.';
      return;
    }
    await login.run(
      () =>
        authLifecycle.authenticate((signal) =>
          passwordLogin(email.trim(), password, signal)
        ),
      async () => {
        // eslint-disable-next-line svelte/no-navigation-without-resolve -- destination() validates local paths and rejects login loops.
        await goto(destination(), { replace: true, refreshAll: true });
      },
      { key: 'password', fallback: 'The control API could not be reached.' }
    );
  }

  async function beginSso(event: MouseEvent) {
    event.preventDefault();
    if (login.busy || !capabilities.oidc_login_enabled) return;
    login.error = '';
    await login.run(
      () => beginOidcLogin(destination(), publicAuthController.signal),
      (authorizationUrl) => {
        window.location.assign(authorizationUrl);
      },
      { key: 'sso', fallback: 'Single sign-on could not be started.' }
    );
  }

  async function samlLogin() {
    if (login.busy) return;
    login.error = '';
    await login.run(
      () => beginSAMLLogin(destination(), publicAuthController.signal),
      (authorizationUrl) => {
        window.location.assign(authorizationUrl);
      },
      { key: 'sso', fallback: 'SAML sign-in could not be started.' }
    );
  }
  async function loadCapabilities() {
    capabilitiesLoading = true;
    capabilitiesError = '';
    const signal = publicAuthController.signal;
    const result = await readResult(() => authenticationCapabilities(signal));
    if (signal.aborted) return;
    capabilitiesLoading = false;
    if (result.ok) capabilities = result.value;
    else {
      capabilitiesError = errorMessage(
        result.error,
        'Sign-in options could not be loaded.'
      );
    }
  }

  onMount(() => {
    void loadCapabilities();
  });

  const branding = useBranding();

  onDestroy(() => {
    publicAuthController.abort();
    authLifecycle.abortAuthenticationWork();
  });
</script>

<PageTitle title="Sign in" />

<SetupFrame>
  <div class="heading">
    <p class="eyebrow">Operator console</p>
    <h1>Sign in</h1>
    <p>Sign in to manage {branding.data?.name ?? 'this installation'}.</p>
  </div>

  {#if sessionExpired}<p class="session-notice" role="status">
      Your session ended. Sign in to continue.
    </p>{/if}

  {#if oidcError}<div class="form-alert" role="alert">{oidcError}</div>{/if}

  {#if login.error}<div class="form-alert" role="alert">{login.error}</div>{/if}

  {#if capabilitiesLoading}
    <p class="capabilities-status" role="status">Loading sign-in options…</p>
  {:else if capabilitiesError}
    <div class="form-alert" role="alert">
      <p>Sign-in options could not be loaded. {capabilitiesError}</p>
      <button
        class="button button-secondary"
        type="button"
        onclick={loadCapabilities}
      >
        Retry
      </button>
    </div>
  {/if}

  {#if capabilities.local_login_enabled}
    <form onsubmit={submit} novalidate>
      <label for="login-email">Email</label>
      <input
        id="login-email"
        type="email"
        autocomplete="username"
        bind:value={email}
        disabled={busy || ssoBusy}
      />
      <label for="login-password">Password</label>
      <input
        id="login-password"
        type="password"
        autocomplete="current-password"
        bind:value={password}
        disabled={busy || ssoBusy}
      />
      <button
        class="button button-primary"
        type="submit"
        disabled={busy || ssoBusy}
        aria-busy={busy}
      >
        {busy ? 'Signing in…' : 'Sign in'}
      </button>
    </form>
  {/if}

  {#if capabilities.oidc_login_enabled}
    {#if capabilities.local_login_enabled}<div class="divider">
        <span>or</span>
      </div>{/if}
    <a
      class="button button-secondary oidc"
      class:busy={ssoBusy || busy}
      href={oidcHref()}
      data-sveltekit-reload
      aria-disabled={ssoBusy || busy}
      aria-busy={ssoBusy}
      onclick={beginSso}
    >
      {ssoBusy ? 'Starting single sign-on…' : 'Continue with single sign-on'}
    </a>
  {:else if !capabilitiesLoading && !capabilitiesError && !capabilities.local_login_enabled && !capabilities.saml_login_enabled}
    <div class="form-alert" role="alert">
      No sign-in method is currently available.
    </div>
  {/if}
  {#if !capabilitiesLoading && !capabilitiesError && capabilities.saml_login_enabled}<button
      class="button button-secondary oidc"
      disabled={busy || ssoBusy}
      onclick={samlLogin}
    >
      Continue with SAML
    </button>{/if}
</SetupFrame>

<style>
  .heading {
    margin-bottom: 1.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.75rem;
    font-weight: 400;
    letter-spacing: -0.031em;
    line-height: 1.1;
  }
  .heading > p:last-child {
    margin: 0.6rem 0 0;
    color: var(--foreground-muted);
  }
  form {
    display: grid;
    gap: 0.5rem;
  }
  label {
    margin-top: 0.4rem;
    font-weight: 500;
  }
  input {
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
  form .button {
    margin-top: 0.85rem;
  }
  .form-alert {
    margin-bottom: 1rem;
    padding: 0.7rem 0.8rem;
    border: 1px solid var(--danger);
    border-radius: var(--radius-control);
    background: var(--danger-soft);
    color: var(--danger);
  }
  .capabilities-status {
    margin: 0;
    color: var(--foreground-muted);
  }
  .session-notice {
    margin: 0 0 1rem;
    padding: 0.7rem 0.8rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: var(--warning-soft);
    color: var(--foreground);
  }
  .divider {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin: 1.25rem 0;
    color: var(--foreground-muted);
  }
  .divider::before,
  .divider::after {
    height: 1px;
    flex: 1;
    background: var(--border);
    content: '';
  }
  .oidc {
    width: 100%;
  }
  .oidc.busy {
    opacity: 0.7;
    pointer-events: none;
  }
</style>
