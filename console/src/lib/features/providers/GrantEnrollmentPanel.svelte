<script lang="ts">
  import { onDestroy } from 'svelte';
  import { copyText } from '$lib/clipboard';
  import { formatDate } from '$lib/format';
  import type { GrantEnrollment } from './grants';

  let {
    enrollment,
    input = $bindable(),
    busy,
    onContinue,
    onCancel
  }: {
    enrollment: GrantEnrollment;
    input: string;
    busy: string;
    onContinue: () => void | Promise<void>;
    onCancel: () => void | Promise<void>;
  } = $props();

  let copied = $state(false);
  let copyError = $state('');
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  onDestroy(() => copyTimer && clearTimeout(copyTimer));

  async function copyAuthorizationURL() {
    if (!(await copyText(enrollment.authorization_url))) {
      copied = false;
      copyError =
        'Clipboard access is unavailable. Copy the authorization URL manually.';
      return;
    }
    copyError = '';
    copied = true;
    if (copyTimer) clearTimeout(copyTimer);
    copyTimer = setTimeout(() => (copied = false), 1800);
  }

  function submit(event: SubmitEvent) {
    event.preventDefault();
    void onContinue();
  }
</script>

<section class="card grant" aria-labelledby="grant-enrollment-heading">
  <p class="eyebrow">Grant enrollment</p>
  <h2 id="grant-enrollment-heading">Sign in upstream</h2>
  <ol>
    <li>
      <p>
        Open the plugin's authorization page, in any browser, and sign in to the
        upstream account this provider should use.
      </p>
      <div class="authorization">
        <code>{enrollment.authorization_url}</code>
        <a
          class="button button-secondary"
          href={enrollment.authorization_url}
          target="_blank"
          rel="noreferrer noopener">Open authorization page</a
        >
        <button
          class="button button-secondary"
          type="button"
          onclick={copyAuthorizationURL}
          aria-label="Copy authorization URL"
          >{copied ? 'Copied' : 'Copy'}</button
        >
        <span class="sr-only" aria-live="polite"
          >{copied ? 'Authorization URL copied to clipboard.' : ''}</span
        >
      </div>
      {#if copyError}<p class="inline-problem" role="alert">{copyError}</p>{/if}
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
