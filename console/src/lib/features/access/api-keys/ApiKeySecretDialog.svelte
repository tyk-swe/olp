<script lang="ts">
  import { onMount } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { listRoutes } from '$lib/api/management/routes';
  import type { ApiKeySecret } from '$lib/api/management/api-keys';
  import { copyText } from '$lib/clipboard';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import {
    SDK_OPTIONS,
    sdkLabel,
    sdkSnippet,
    type ApiKeySdk
  } from './sdkExamples';

  let {
    secret,
    context,
    preferredRoute,
    onClose
  }: {
    secret: ApiKeySecret;
    context: 'created' | 'rotated';
    preferredRoute?: string;
    onClose: () => void;
  } = $props();

  let sdk = $state<ApiKeySdk>('openai');
  let endpoint = $state('');
  let copied = $state('');
  let copyError = $state('');
  let testState = $state<'idle' | 'running' | 'passed' | 'failed'>('idle');
  let testMessage = $state('');
  const routes = createQuery(() => ({
    queryKey: ['routes'],
    queryFn: listRoutes
  }));
  const routeSlug = $derived(
    preferredRoute ?? routes.data?.[0]?.slug ?? 'default'
  );
  const snippet = $derived(sdkSnippet(sdk, secret.secret, endpoint, routeSlug));

  onMount(() => {
    endpoint = window.location.origin;
  });

  async function copy(value: string, label: string) {
    if (!(await copyText(value))) {
      copied = '';
      copyError = 'Clipboard access is unavailable. Copy the value manually.';
      return;
    }
    copyError = '';
    copied = label;
    setTimeout(() => {
      if (copied === label) copied = '';
    }, 1800);
  }

  function selectSdk(option: ApiKeySdk) {
    sdk = option;
    testState = 'idle';
    testMessage = '';
  }

  function moveSdkTab(event: KeyboardEvent, index: number) {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const next =
      event.key === 'Home'
        ? 0
        : event.key === 'End'
          ? SDK_OPTIONS.length - 1
          : (index +
              (event.key === 'ArrowRight' ? 1 : -1) +
              SDK_OPTIONS.length) %
            SDK_OPTIONS.length;
    selectSdk(SDK_OPTIONS[next]);
    requestAnimationFrame(() =>
      document.getElementById(`sdk-tab-${SDK_OPTIONS[next]}`)?.focus()
    );
  }

  async function testGeneratedKey() {
    testState = 'running';
    testMessage = '';
    try {
      let response: Response;
      if (sdk === 'anthropic') {
        response = await fetch(`${endpoint}/anthropic/v1/messages`, {
          method: 'POST',
          headers: {
            'content-type': 'application/json',
            'x-api-key': secret.secret,
            'anthropic-version': '2023-06-01'
          },
          body: JSON.stringify({
            model: routeSlug,
            max_tokens: 16,
            messages: [{ role: 'user', content: 'Connection test' }]
          })
        });
      } else if (sdk === 'gemini') {
        response = await fetch(
          `${endpoint}/gemini/v1beta/models/${encodeURIComponent(routeSlug)}:generateContent`,
          {
            method: 'POST',
            headers: {
              'content-type': 'application/json',
              'x-goog-api-key': secret.secret
            },
            body: JSON.stringify({
              contents: [
                { role: 'user', parts: [{ text: 'Connection test' }] }
              ],
              generationConfig: { maxOutputTokens: 16 }
            })
          }
        );
      } else {
        response = await fetch(`${endpoint}/openai/v1/responses`, {
          method: 'POST',
          headers: {
            'content-type': 'application/json',
            authorization: `Bearer ${secret.secret}`
          },
          body: JSON.stringify({
            model: routeSlug,
            input: 'Connection test',
            max_output_tokens: 16
          })
        });
      }
      if (!response.ok) {
        let detail = `Request failed (${response.status}).`;
        try {
          const problem = (await response.json()) as {
            detail?: string;
            error?: { message?: string };
          };
          detail = problem.detail ?? problem.error?.message ?? detail;
        } catch {
          // The status remains enough when an intermediary returns no JSON.
        }
        throw new Error(detail);
      }
      await response.body?.cancel();
      testState = 'passed';
      testMessage = `${sdkLabel(sdk)} request succeeded through route ${routeSlug}.`;
    } catch (error) {
      testState = 'failed';
      testMessage =
        error instanceof Error
          ? error.message
          : 'The generated key test failed.';
    }
  }
</script>

<SecretDialog
  eyebrow={context === 'created' ? 'Key created' : 'Key rotated'}
  title="Copy this secret now."
  description="The full proxy key is shown once. It cannot be retrieved after this panel closes."
  size="wide"
  {onClose}
>
  {#snippet children(close)}
    <span class="secret-icon" aria-hidden="true"
      ><NavIcon name="key" size={26} /></span
    >
    <div class="secret-row">
      <code>{secret.secret}</code><button
        class="button button-secondary"
        type="button"
        onclick={() => copy(secret.secret, 'secret')}
        >{copied === 'secret' ? 'Copied' : 'Copy key'}</button
      >
    </div>
    {#if copyError}<div class="inline-problem" role="alert">
        {copyError}
      </div>{/if}
    <div class="snippet-heading">
      <div>
        <strong>Test with a vendor SDK</strong><small
          >Route slugs are sent as the model.</small
        >
      </div>
      <div class="tabs" role="tablist" aria-label="SDK language">
        {#each SDK_OPTIONS as option, index (option)}<button
            id={`sdk-tab-${option}`}
            class:active={sdk === option}
            role="tab"
            aria-selected={sdk === option}
            aria-controls={`sdk-panel-${option}`}
            tabindex={sdk === option ? 0 : -1}
            type="button"
            onclick={() => selectSdk(option)}
            onkeydown={(event) => moveSdkTab(event, index)}
            >{option === 'openai'
              ? 'OpenAI Python'
              : option === 'anthropic'
                ? 'Anthropic TS'
                : 'Gemini TS'}</button
          >{/each}
      </div>
    </div>
    <div
      id={`sdk-panel-${sdk}`}
      role="tabpanel"
      aria-labelledby={`sdk-tab-${sdk}`}
    >
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <pre tabindex="0"><code>{snippet}</code></pre>
    </div>
    {#if testMessage}<div
        class:success={testState === 'passed'}
        class:danger={testState === 'failed'}
        class="key-test-result"
        role={testState === 'failed' ? 'alert' : 'status'}
      >
        {testMessage}
      </div>{/if}
    <div class="secret-actions">
      <button
        class="button button-secondary"
        type="button"
        onclick={() => copy(snippet, 'snippet')}
        >{copied === 'snippet' ? 'Snippet copied' : 'Copy snippet'}</button
      ><button
        class="button button-secondary"
        type="button"
        onclick={testGeneratedKey}
        disabled={testState === 'running'}
        >{testState === 'running' ? 'Testing…' : 'Run connection test'}</button
      ><button
        class="button button-primary"
        type="button"
        data-autofocus
        onclick={close}>I have saved the key</button
      >
    </div>
  {/snippet}
</SecretDialog>

<style>
  .secret-icon {
    display: grid;
    width: 2.75rem;
    height: 2.75rem;
    place-items: center;
    margin-bottom: 1rem;
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  .secret-row {
    display: flex;
    align-items: stretch;
    margin: 1rem 0;
    overflow: hidden;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
  }
  .secret-row code {
    min-width: 0;
    flex: 1;
    overflow-x: auto;
    padding: 0.8rem;
    font-size: 0.82rem;
  }
  .secret-row .button {
    border-width: 0 0 0 1px;
    border-radius: 0;
  }
  .snippet-heading {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: 1rem;
    margin-top: 1.5rem;
  }
  .snippet-heading > div:first-child {
    display: grid;
  }
  .snippet-heading small {
    color: var(--foreground-muted);
  }
  .tabs {
    display: flex;
    gap: 0.25rem;
  }
  .tabs button {
    min-height: 2.5rem;
    padding: 0.5rem 0.65rem;
    border: 0;
    border-radius: 0.375rem;
    background: transparent;
    color: var(--foreground-muted);
  }
  .tabs button.active {
    background: var(--accent-soft);
    color: var(--accent-strong);
    font-weight: 700;
  }
  pre {
    max-height: 18rem;
    overflow: auto;
    padding: 1rem;
    border-radius: 0.375rem;
    background: var(--code-bg);
    color: var(--code-foreground);
  }
  pre code {
    font-size: 0.76rem;
    white-space: pre;
  }
  code {
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
  }
  .key-test-result {
    margin-top: 0.65rem;
    padding: 0.7rem 0.8rem;
    border: 1px solid currentColor;
    border-radius: 0.375rem;
    font-size: 0.78rem;
  }
  .key-test-result.success {
    color: var(--success);
    background: var(--success-soft);
  }
  .key-test-result.danger {
    color: var(--danger);
    background: var(--danger-soft);
  }
  .secret-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.65rem;
  }
  @media (forced-colors: active) {
    pre {
      border: 1px solid CanvasText;
      background: Canvas;
      color: CanvasText;
    }
  }
  @media (max-width: 48rem) {
    .snippet-heading,
    .secret-row,
    .secret-actions {
      display: grid;
    }
    .tabs {
      overflow-x: auto;
    }
    .secret-row .button {
      border-width: 1px 0 0;
    }
  }
</style>
