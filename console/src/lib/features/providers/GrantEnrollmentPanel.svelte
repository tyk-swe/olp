<script lang="ts">
  import { onDestroy } from 'svelte';
  import { copyText } from '$lib/clipboard';
  import { formatDate } from '$lib/format';
  import type { GrantEnrollment } from './api/grants';

  let {
    enrollment,
    input = $bindable(),
    busy,
    onContinue,
    onPoll,
    onCancel
  }: {
    enrollment: GrantEnrollment;
    input: string;
    busy: string;
    onContinue: () => void | Promise<void>;
    /** Asks where a device authorization stands; returns the seconds to wait
     * before asking again, or null once the enrollment ended. */
    onPoll: () => Promise<number | null>;
    onCancel: () => void | Promise<void>;
  } = $props();

  const device = $derived(enrollment.device);

  // Status requests drive a device authorization's polling: each waits the
  // interval OLP last reported, and none follows once the enrollment ended or
  // the panel is gone.
  $effect(() => {
    if (!device) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const wait = (seconds: number) => {
      timer = setTimeout(async () => {
        const next = await onPoll();
        if (!stopped && next !== null) wait(next);
      }, seconds * 1000);
    };
    wait(device.interval);
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  });

  /** What was copied last, such as "authorization URL". */
  let copied = $state('');
  let copyError = $state('');
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  onDestroy(() => copyTimer && clearTimeout(copyTimer));

  async function copy(value: string, what: string) {
    if (!(await copyText(value))) {
      copied = '';
      copyError = `Clipboard access is unavailable. Copy the ${what} manually.`;
      return;
    }
    copyError = '';
    copied = what;
    if (copyTimer) clearTimeout(copyTimer);
    copyTimer = setTimeout(() => (copied = ''), 1800);
  }

  function submit(event: SubmitEvent) {
    event.preventDefault();
    void onContinue();
  }
</script>

{#snippet copyButton(value: string, what: string)}
  <button
    class="button button-secondary"
    type="button"
    onclick={() => copy(value, what)}
    aria-label={`Copy ${what}`}>{copied === what ? 'Copied' : 'Copy'}</button
  >
{/snippet}

<section class="card grant" aria-labelledby="grant-enrollment-heading">
  <p class="eyebrow">Grant enrollment</p>
  <h2 id="grant-enrollment-heading">Sign in upstream</h2>
  {#if device}
    <ol>
      <li>
        <p>
          Open the verification page, on any device, and sign in to the upstream
          account this provider should use.
        </p>
        <div class="authorization">
          <code>{device.verification_url}</code>
          <a
            class="button button-secondary"
            href={device.verification_url}
            target="_blank"
            rel="noreferrer noopener">Open verification page</a
          >
          {@render copyButton(device.verification_url, 'verification URL')}
        </div>
      </li>
      <li>
        <p>Enter this code there, and approve the device.</p>
        <div class="authorization">
          <code class="user-code">{device.user_code}</code>
          {@render copyButton(device.user_code, 'user code')}
        </div>
      </li>
    </ol>
    <p class="waiting" role="status">
      Waiting for approval upstream. OLP continues once the upstream reports it,
      before {formatDate(enrollment.expires_at)}.
    </p>
    <div class="actions">
      <button
        class="button button-secondary"
        type="button"
        disabled={Boolean(busy)}
        onclick={() => onCancel()}>Cancel sign-in</button
      >
    </div>
  {:else}
    <ol>
      <li>
        <p>
          Open the plugin's authorization page, in any browser, and sign in to
          the upstream account this provider should use.
        </p>
        <div class="authorization">
          <code>{enrollment.authorization_url}</code>
          <a
            class="button button-secondary"
            href={enrollment.authorization_url}
            target="_blank"
            rel="noreferrer noopener">Open authorization page</a
          >
          {@render copyButton(
            enrollment.authorization_url ?? '',
            'authorization URL'
          )}
        </div>
      </li>
      <li>
        <form onsubmit={submit} aria-busy={busy === 'grant'}>
          <div class="form-field">
            <label for="grant-input"
              >Paste back the callback URL or code the upstream returned</label
            ><input
              id="grant-input"
              aria-describedby="grant-input-help"
              autocomplete="off"
              spellcheck="false"
              bind:value={input}
              required
            /><small id="grant-input-help"
              >After you sign in, the upstream sends your browser to a callback
              URL that may not load: copy it from the address bar. Or copy the
              code the upstream displays. Continue before {formatDate(
                enrollment.expires_at
              )}; each sign-in can be used once.</small
            >
          </div>
          <div class="actions">
            <button
              class="button button-primary"
              type="submit"
              disabled={Boolean(busy)}
              >{busy === 'grant' ? 'Enrolling the grant…' : 'Continue'}</button
            >
            <button
              class="button button-secondary"
              type="button"
              disabled={Boolean(busy)}
              onclick={() => onCancel()}>Cancel sign-in</button
            >
          </div>
        </form>
      </li>
    </ol>
  {/if}
  <span class="sr-only" aria-live="polite"
    >{copied ? `Copied the ${copied} to the clipboard.` : ''}</span
  >
  {#if copyError}<p class="inline-problem" role="alert">{copyError}</p>{/if}
</section>

<style>
  .grant {
    padding: 1.5rem;
  }
  h2 {
    margin: 0.2rem 0 0.9rem;
  }
  ol {
    display: grid;
    gap: 1.1rem;
    margin: 0;
    padding-left: 1.25rem;
  }
  p {
    margin: 0 0 0.6rem;
  }
  .authorization {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.6rem;
  }
  .authorization code {
    flex: 1 1 100%;
    padding: 0.6rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    font-size: var(--text-caption);
    overflow-wrap: anywhere;
  }
  .authorization code.user-code {
    flex: 0 1 auto;
    font-size: 1.25rem;
    font-weight: 600;
    letter-spacing: 0.12em;
  }
  .waiting {
    margin: 1.1rem 0 0;
  }
  form {
    display: grid;
    gap: 0.4rem;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    margin-top: 0.4rem;
  }
</style>
