<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { onMount } from 'svelte';
  import { currentSession, logout, type SessionUser } from '$lib/api/auth';
  import { ApiProblem } from '$lib/api/http';
  import { clearCsrfToken } from '$lib/api/session';
  import { getSetupStatus } from '$lib/api/setup';
  import AppShell from '$lib/components/AppShell.svelte';

  let { children } = $props();
  let user = $state<SessionUser | null>(null);
  let sessionState = $state<'checking' | 'ready' | 'unavailable'>('checking');
  let sessionError = $state('');
  let signOutError = $state('');
  let signingOut = $state(false);
  let activeController: AbortController | undefined;

  function loginDestination() {
    const returnTo = `${page.url.pathname}${page.url.search}${page.url.hash}`;
    return `${resolve('/login')}?return_to=${encodeURIComponent(returnTo)}`;
  }

  async function requireSession() {
    activeController?.abort();
    activeController = new AbortController();
    const { signal } = activeController;
    sessionState = 'checking';
    sessionError = '';
    try {
      const session = await currentSession(signal);
      user = session.user;
      sessionState = 'ready';
    } catch (error) {
      if (signal.aborted) return;
      clearCsrfToken();
      if (error instanceof ApiProblem && error.problem.status === 401) {
        try {
          const setup = await getSetupStatus(signal);
          if (signal.aborted) return;
          // The destination combines a resolved local login route and an encoded same-origin return path.
          // eslint-disable-next-line svelte/no-navigation-without-resolve
          await goto(setup.setup_required ? resolve('/setup') : loginDestination(), {
            replaceState: true
          });
          return;
        } catch (setupError) {
          if (signal.aborted) return;
          sessionError =
            setupError instanceof Error
              ? setupError.message
              : 'The installation state could not be loaded.';
        }
      } else {
        sessionError =
          error instanceof Error ? error.message : 'The current session could not be loaded.';
      }
      sessionState = 'unavailable';
    }
  }

  async function signOut() {
    if (signingOut) return;
    signingOut = true;
    signOutError = '';
    try {
      await logout();
      await goto(resolve('/login'), { replaceState: true });
    } catch (error) {
      signOutError =
        error instanceof Error
          ? error.message
          : 'The sign-out request could not be completed.';
    } finally {
      signingOut = false;
    }
  }

  onMount(() => {
    void requireSession();
    return () => activeController?.abort();
  });
</script>

{#if sessionState === 'checking'}
  <main class="session-gate" aria-busy="true">
    <p role="status"><span aria-hidden="true"></span>Verifying your session…</p>
  </main>
{:else if sessionState === 'unavailable' || !user}
  <main class="session-gate">
    <div class="problem-banner" role="alert">
      <div>
        <strong>Session verification unavailable</strong>
        <p>{sessionError} Protected console content has not been loaded.</p>
      </div>
      <button class="button button-secondary" type="button" onclick={requireSession}>Retry</button>
    </div>
  </main>
{:else}
  <AppShell {user} {signingOut} {signOutError} onSignOut={signOut}>
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

  .session-gate .problem-banner {
    width: min(100%, 42rem);
  }

  @keyframes session-spin {
    to { transform: rotate(360deg); }
  }
</style>
