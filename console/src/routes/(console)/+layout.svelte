<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { onMount } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import {
    authenticationCapabilities,
    currentSession,
    logout,
    type AuthenticationCapabilities
  } from '#lib/session/api.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { getSetupStatus } from '#lib/features/access/setup/api.js';
  import { authLifecycle } from '#lib/session/lifecycle.js';
  import { sessionKeys } from '#lib/session/sessionKeys.js';
  import type { AuthenticationSnapshot } from '#lib/session/state.js';
  import AppShell from '#lib/shell/AppShell.svelte';
  import BrandingHead from '#lib/shell/branding/BrandingHead.svelte';
  import BrandMark from '#lib/shell/BrandMark.svelte';

  let { children } = $props();
  const queryClient = useQueryClient();
  authLifecycle.markProtectedBoundaryChecking();
  let authentication = $state<AuthenticationSnapshot>(authLifecycle.snapshot());
  let pendingCapabilities: AuthenticationCapabilities | null = null;
  const exit = useAction(undefined, 'authentication');

  function loginDestination(sessionExpired = false) {
    const returnTo = page.url.pathname + page.url.search + page.url.hash;
    const expired = sessionExpired ? '&reason=expired' : '';
    return `${resolve('login')}?return_to=${encodeURIComponent(returnTo)}${expired}`;
  }

  async function signOut() {
    await exit.run(
      () => authLifecycle.signOut((signal) => logout(signal), resolve('login')),
      undefined,
      { fallback: 'The sign-out request could not be completed.' }
    );
  }

  onMount(() => {
    const unsubscribe = authLifecycle.subscribe((snapshot) => {
      if (snapshot.phase === 'authenticated' && pendingCapabilities) {
        if (
          queryClient.getQueryData(sessionKeys.serviceCapabilities) ===
          undefined
        ) {
          queryClient.setQueryData(
            sessionKeys.serviceCapabilities,
            pendingCapabilities
          );
        }
        pendingCapabilities = null;
      }
      authentication = snapshot;
    });
    const unregister = authLifecycle.registerBoundary({
      async loadSession(signal) {
        // Capabilities are installation-wide, so they can fly beside the
        // session load; an already-authenticated passive revalidation must
        // not fetch them again.
        const capabilities =
          authLifecycle.snapshot().phase === 'authenticated'
            ? Promise.resolve(null)
            : authenticationCapabilities(signal).catch(() => null);
        const [session, fetched] = await Promise.all([
          currentSession(signal),
          capabilities
        ]);
        pendingCapabilities = fetched;
        return session;
      },
      async unauthenticatedDestination(signal, sessionExpired) {
        const setup = await getSetupStatus(signal);

        return setup.setup_required
          ? resolve('setup')
          : loginDestination(sessionExpired);
      },
      loginDestination,
      async navigate(destination) {
        // eslint-disable-next-line svelte/no-navigation-without-resolve -- every destination is a resolved local route or a validated same-origin relative return path.
        await goto(destination, { replace: true });
      }
    });

    const revalidate = () => {
      if (
        document.visibilityState === 'visible' &&
        authLifecycle.snapshot().phase === 'authenticated'
      ) {
        void authLifecycle.validateSession({ passive: true });
      }
    };
    window.addEventListener('focus', revalidate);
    document.addEventListener('visibilitychange', revalidate);
    void authLifecycle.validateSession();

    return () => {
      window.removeEventListener('focus', revalidate);
      document.removeEventListener('visibilitychange', revalidate);
      unregister();
      unsubscribe();
    };
  });
</script>

{#if authentication.phase === 'checking' || authentication.phase === 'transitioning'}
  <main class="session-gate" aria-busy="true">
    <p role="status"><BrandMark size={36} animated />Verifying your session…</p>
  </main>
{:else if authentication.phase !== 'authenticated' || !authentication.user}
  <main class="session-gate" aria-busy={authentication.phase === 'anonymous'}>
    {#if authentication.phase === 'anonymous'}
      <!-- Anonymous here means the redirect to login is still being resolved. -->
      <p role="status">
        <BrandMark size={36} animated />Verifying your session…
      </p>
    {:else if authentication.phase === 'unavailable'}
      <div class="problem-banner" role="alert">
        <div>
          <strong>Session verification unavailable</strong>
          <p>
            {authentication.error} Protected console content has not been loaded.
          </p>
        </div>
        <button
          class="button button-secondary"
          type="button"
          onclick={() => authLifecycle.validateSession()}>Retry</button
        >
      </div>
    {/if}
  </main>
{:else}
  {#key authLifecycle.principalPartition(authentication.user)}
    <BrandingHead />
    <AppShell
      user={authentication.user}
      signingOut={exit.busy}
      signOutError={exit.error || authentication.principalExitError}
      onSignOut={signOut}
    >
      {#if authentication.error}
        <div class="problem-banner" role="alert">
          <div>
            <strong>Session verification unavailable</strong>
            <p>
              {authentication.error} Previously loaded content is shown. Changes require
              successful verification.
            </p>
          </div>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => authLifecycle.validateSession()}>Retry</button
          >
        </div>
      {/if}
      {@render children()}
    </AppShell>
  {/key}
{/if}

<style>
  .session-gate {
    display: grid;
    min-height: 100dvh;
    padding: 2rem;
    place-items: center;
    background: var(--canvas);
  }

  .session-gate > p {
    display: flex;
    min-height: 2.75rem;
    flex-direction: column;
    align-items: center;
    gap: 1rem;
    color: var(--foreground-muted);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    letter-spacing: -0.24px;
    text-transform: uppercase;
    animation: fade var(--dur-enter) var(--ease-out) 120ms backwards;
  }

  .session-gate .problem-banner {
    width: min(100%, 42rem);
  }
</style>
