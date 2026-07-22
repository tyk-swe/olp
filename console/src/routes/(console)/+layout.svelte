<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { onMount } from 'svelte';
  import { currentSession, logout } from '$lib/api/auth';
  import { getSetupStatus } from '$lib/api/setup';
  import { authLifecycle, type AuthenticationSnapshot } from '$lib/auth/lifecycle';
  import { currentRelativeDestination } from '$lib/auth/paths';
  import AppShell from '$lib/components/AppShell.svelte';

  let { children } = $props();
  authLifecycle.markProtectedBoundaryChecking();
  let authentication = $state<AuthenticationSnapshot>(authLifecycle.snapshot());
  let signOutError = $state('');
  let signingOut = $state(false);

  function loginDestination() {
    const returnTo = currentRelativeDestination(page.url);
    return `${resolve('/login')}?return_to=${encodeURIComponent(returnTo)}`;
  }

  async function signOut() {
    if (signingOut) return;
    signingOut = true;
    signOutError = '';
    try {
      await authLifecycle.signOut((signal) => logout(signal), resolve('/login'));
    } catch (error) {
      signOutError =
        error instanceof Error ? error.message : 'The sign-out request could not be completed.';
    } finally {
      signingOut = false;
    }
  }

  onMount(() => {
    const unsubscribe = authLifecycle.subscribe((snapshot) => {
      authentication = snapshot;
    });
    const unregister = authLifecycle.registerBoundary({
      loadSession: currentSession,
      async unauthenticatedDestination(signal) {
        const setup = await getSetupStatus(signal);
        return setup.setup_required ? resolve('/setup') : loginDestination();
      },
      loginDestination,
      async navigate(destination) {
        // Every destination is either a resolved local route or a validated
        // same-origin relative return path constructed above.
        // eslint-disable-next-line svelte/no-navigation-without-resolve
        await goto(destination, { replaceState: true });
      }
    });

    const revalidate = () => {
      if (document.visibilityState === 'visible' && authLifecycle.snapshot().phase === 'authenticated') {
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
    <p role="status"><span aria-hidden="true"></span>Verifying your session…</p>
  </main>
{:else if authentication.phase !== 'authenticated' || !authentication.user}
  <main class="session-gate">
    {#if authentication.phase === 'unavailable'}
      <div class="problem-banner" role="alert">
        <div>
          <strong>Session verification unavailable</strong>
          <p>{authentication.error} Protected console content has not been loaded.</p>
        </div>
        <button class="button button-secondary" type="button" onclick={() => authLifecycle.validateSession()}>Retry</button>
      </div>
    {/if}
  </main>
{:else}
  <AppShell user={authentication.user} {signingOut} signOutError={signOutError || authentication.principalExitError} onSignOut={signOut}>
    {@render children()}
  </AppShell>
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
    align-items: center;
    gap: 0.65rem;
    color: var(--foreground-muted);
  }

  .session-gate > p span {
    width: 0.9rem;
    height: 0.9rem;
    border: 2px solid var(--border-strong);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: session-spin 700ms linear infinite;
  }

  .session-gate .problem-banner { width: min(100%, 42rem); }

  @keyframes session-spin { to { transform: rotate(360deg); } }
</style>
