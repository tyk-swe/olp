<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { ApiProblem } from '$lib/api/http';
  import { login } from '$lib/api/auth';
  import SetupFrame from '$lib/features/setup/SetupFrame.svelte';

  let email = $state('');
  let password = $state('');
  let busy = $state(false);
  let message = $state('');

  function safeReturnTo() {
    const requested = page.url.searchParams.get('return_to');
    if (!requested || !requested.startsWith('/') || requested.startsWith('//')) return resolve('/');
    try {
      const parsed = new URL(requested, page.url.origin);
      if (parsed.origin !== page.url.origin) return resolve('/');
      return `${parsed.pathname}${parsed.search}${parsed.hash}`;
    } catch {
      return resolve('/');
    }
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    message = '';
    if (!email.trim() || !password) {
      message = 'Enter your email and password.';
      return;
    }
    busy = true;
    try {
      await login(email.trim(), password);
      // safeReturnTo rejects cross-origin and protocol-relative destinations.
      // eslint-disable-next-line svelte/no-navigation-without-resolve
      await goto(safeReturnTo(), { replaceState: true, invalidateAll: true });
    } catch (error) {
      message =
        error instanceof ApiProblem
          ? (error.problem.detail ?? error.problem.title)
          : 'The control API could not be reached.';
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head><title>Sign in · OpenLLMProxy</title></svelte:head>

<SetupFrame>
  <div class="heading">
    <p class="eyebrow">Operator console</p>
    <h1>Sign in</h1>
    <p>Use your local account or continue with the configured identity provider.</p>
  </div>

  {#if message}<div class="form-alert" role="alert">{message}</div>{/if}

  <form onsubmit={submit} novalidate>
    <label for="login-email">Email</label>
    <input id="login-email" type="email" autocomplete="username" bind:value={email} disabled={busy} />
    <label for="login-password">Password</label>
    <input id="login-password" type="password" autocomplete="current-password" bind:value={password} disabled={busy} />
    <button class="button button-primary" type="submit" disabled={busy} aria-busy={busy}>
      {busy ? 'Signing in…' : 'Sign in'}
    </button>
  </form>

  <div class="divider"><span>or</span></div>
  <a class="button button-secondary oidc" href="/api/v1/oidc/login" data-sveltekit-reload>Continue with single sign-on</a>
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
  .divider { display: flex; align-items: center; gap: 0.75rem; margin: 1.25rem 0; color: var(--foreground-muted); }
  .divider::before, .divider::after { height: 1px; flex: 1; background: var(--border); content: ''; }
  .oidc { width: 100%; }
</style>
