<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { onDestroy, onMount } from 'svelte';
  import {
    authenticationCapabilities,
    beginOidcLogin,
    login,
    type AuthenticationCapabilities
  } from '$lib/api/auth';
  import { ApiProblem } from '$lib/api/http';
  import { authLifecycle } from '$lib/auth/lifecycle';
  import { relativeReturnTo } from '$lib/auth/relativeReturnTo';
  import SetupFrame from '$lib/features/setup/SetupFrame.svelte';

  let email = $state('');
  let password = $state('');
  let busy = $state(false);
  let oidcBusy = $state(false);
  let capabilitiesLoading = $state(true);
  let capabilities = $state<AuthenticationCapabilities>({
    local_login_enabled: false,
    oidc_login_enabled: false
  });
  let message = $state('');

  function destination() {
    return relativeReturnTo(page.url.searchParams.get('return_to'), page.url.origin);
  }

  function oidcHref() {
    return `/api/v1/oidc/login?return_to=${encodeURIComponent(destination())}`;
  }

  function problemMessage(error: unknown, fallback: string) {
    return error instanceof ApiProblem
      ? (error.problem.detail ?? error.problem.title)
      : error instanceof Error
        ? error.message
        : fallback;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || oidcBusy) return;
    message = '';
    if (!capabilities.local_login_enabled) {
      message = 'Password-based sign-in is not available for this installation.';
      return;
    }
    if (!email.trim() || !password) {
      message = 'Enter your email and password.';
      return;
    }
    busy = true;
    try {
      await authLifecycle.authenticate((signal) => login(email.trim(), password, signal));
      // destination() rejects external, ambiguous, malformed, and login-loop values.
      // eslint-disable-next-line svelte/no-navigation-without-resolve
      await goto(destination(), { replaceState: true, invalidateAll: true });
    } catch (error) {
      message = problemMessage(error, 'The control API could not be reached.');
    } finally {
      busy = false;
    }
  }

  async function beginSso(event: MouseEvent) {
    event.preventDefault();
    if (oidcBusy || busy || !capabilities.oidc_login_enabled) return;
    message = '';
    oidcBusy = true;
    try {
      const authorizationUrl = await beginOidcLogin(destination());
      window.location.assign(authorizationUrl);
    } catch (error) {
      message = problemMessage(error, 'Single sign-on could not be started.');
    } finally {
      oidcBusy = false;
    }
  }

  onMount(() => {
    const controller = new AbortController();
    void authenticationCapabilities(controller.signal)
      .then((value) => {
        capabilities = value;
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        message = problemMessage(error, 'Sign-in options could not be loaded.');
      })
      .finally(() => {
        if (!controller.signal.aborted) capabilitiesLoading = false;
    });
    return () => controller.abort();
  });

  onDestroy(() => authLifecycle.abortAuthenticationWork());
</script>

<svelte:head><title>Sign in · OpenLLMProxy</title></svelte:head>

<SetupFrame>
  <div class="heading">
    <p class="eyebrow">Operator console</p>
    <h1>Sign in</h1>
    <p>Choose an authentication method enabled for this installation.</p>
  </div>

  {#if message}<div class="form-alert" role="alert">{message}</div>{/if}

  {#if capabilitiesLoading}
    <p class="capabilities-status" role="status">Loading sign-in options…</p>
  {/if}

  {#if capabilities.local_login_enabled}
    <form onsubmit={submit} novalidate>
      <label for="login-email">Email</label>
      <input id="login-email" type="email" autocomplete="username" bind:value={email} disabled={busy || oidcBusy} />
      <label for="login-password">Password</label>
      <input id="login-password" type="password" autocomplete="current-password" bind:value={password} disabled={busy || oidcBusy} />
      <button class="button button-primary" type="submit" disabled={busy || oidcBusy} aria-busy={busy}>
        {busy ? 'Signing in…' : 'Sign in'}
      </button>
    </form>
  {/if}

  {#if capabilities.oidc_login_enabled}
    {#if capabilities.local_login_enabled}<div class="divider"><span>or</span></div>{/if}
    <a
      class="button button-secondary oidc"
      class:busy={oidcBusy || busy}
      href={oidcHref()}
      data-sveltekit-reload
      aria-disabled={oidcBusy || busy}
      aria-busy={oidcBusy}
      onclick={beginSso}
    >{oidcBusy ? 'Starting single sign-on…' : 'Continue with single sign-on'}</a>
  {:else if !capabilitiesLoading && !capabilities.local_login_enabled && !message}
    <div class="form-alert" role="alert">No sign-in method is currently available.</div>
  {/if}
</SetupFrame>

<style>
  .heading { margin-bottom: 1.6rem; }
  h1 { margin: 0; font-size: clamp(1.8rem, 4vw, 2.2rem); letter-spacing: -0.035em; }
  .heading > p:last-child { margin: 0.6rem 0 0; color: var(--foreground-muted); }
  form { display: grid; gap: 0.5rem; }
  label { margin-top: 0.4rem; font-weight: 700; }
  input { min-height: 2.5rem; padding: 0.5rem 0.7rem; border: 1px solid var(--border-strong); border-radius: 0.375rem; background: var(--surface); color: var(--foreground); }
  form .button { margin-top: 0.85rem; }
  .form-alert { margin-bottom: 1rem; padding: 0.7rem 0.8rem; border: 1px solid var(--danger); border-radius: 0.375rem; background: var(--danger-soft); color: var(--danger); }
  .capabilities-status { margin: 0; color: var(--foreground-muted); }
  .divider { display: flex; align-items: center; gap: 0.75rem; margin: 1.25rem 0; color: var(--foreground-muted); }
  .divider::before, .divider::after { height: 1px; flex: 1; background: var(--border); content: ''; }
  .oidc { width: 100%; }
  .oidc.busy { opacity: 0.7; pointer-events: none; }
</style>
