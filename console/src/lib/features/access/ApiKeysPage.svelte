<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onDestroy, onMount } from 'svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { ApiProblem } from '$lib/api/http';
  import { dateTimeLocalValue } from '$lib/features/operations/format';
  import {
    createApiKey,
    listApiKeyPage,
    revokeApiKey,
    rotateApiKey,
    updateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '$lib/api/management/api-keys';
  import { listRoutes } from '$lib/api/management/routes';
  import type { ApiKeyListState } from './apiKeyListState';
  import { validateApiKey } from './keyValidation';

  let {
    isNew = false,
    listState
  }: {
    isNew?: boolean;
    listState: ApiKeyListState;
  } = $props();
  let editing = $state<ApiKey | null>(null);
  const isForm = $derived(isNew || editing !== null);
  const queryClient = useQueryClient();
  const keys = createQuery(() => ({ queryKey: ['api-key-page', listState.cursor ?? 'first'], queryFn: () => listApiKeyPage(listState.cursor), enabled: !isNew }));

  let name = $state('');
  let scopes = $state<string[]>(['inference']);
  let allowedRoutes = $state<string[]>([]);
  let requestsPerMinute = $state('');
  let tokensPerMinute = $state('');
  let maxConcurrency = $state('');
  let expiresAt = $state('');
  let errors = $state<Record<string, string>>({});
  let errorMessage = $state('');
  let notice = $state('');
  let busy = $state('');
  let secret = $state<ApiKeySecret | null>(null);
  let secretContext = $state<'created' | 'rotated'>('created');
  let sdk = $state<'openai' | 'anthropic' | 'gemini'>('openai');
  let endpoint = $state('');
  let copied = $state('');
  let testState = $state<'idle' | 'running' | 'passed' | 'failed'>('idle');
  let testMessage = $state('');
  const sdkOptions = ['openai', 'anthropic', 'gemini'] as const;
  const routes = createQuery(() => ({ queryKey: ['routes'], queryFn: listRoutes, enabled: isForm || secret !== null }));

  onMount(() => { endpoint = window.location.origin; });
  onDestroy(() => { secret = null; copied = ''; });

  const routeSlug = $derived(allowedRoutes[0] ?? routes.data?.[0]?.slug ?? 'default');
  const snippet = $derived.by(() => {
    if (!secret) return '';
    if (sdk === 'anthropic') return `import Anthropic from "@anthropic-ai/sdk";\n\nconst client = new Anthropic({\n  apiKey: "${secret.secret}",\n  baseURL: "${endpoint}/anthropic",\n});\n\nconst message = await client.messages.create({\n  model: "${routeSlug}",\n  max_tokens: 512,\n  messages: [{ role: "user", content: "Hello" }],\n});`;
    if (sdk === 'gemini') return `import { GoogleGenAI } from "@google/genai";\n\nconst ai = new GoogleGenAI({\n  apiKey: "${secret.secret}",\n  apiVersion: "v1beta",\n  httpOptions: {\n    baseUrl: "${endpoint}/gemini",\n    apiVersion: "v1beta",\n    retryOptions: { attempts: 1 },\n  },\n});\n\nconst response = await ai.models.generateContent({\n  model: "${routeSlug}",\n  contents: "Hello",\n});`;
    return `from openai import OpenAI\n\nclient = OpenAI(\n    api_key="${secret.secret}",\n    base_url="${endpoint}/openai/v1",\n)\n\nresponse = client.responses.create(\n    model="${routeSlug}",\n    input="Hello",\n)`;
  });

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error ? error.message : 'The control API could not complete the request.';
  }

  function toggle(list: string[], value: string, checked: boolean) {
    return checked ? [...new Set([...list, value])] : list.filter((item) => item !== value);
  }

  function numberValue(value: string) {
    return value ? Number(value) : undefined;
  }

  function resetForm() {
    name = '';
    scopes = ['inference'];
    allowedRoutes = [];
    requestsPerMinute = '';
    tokensPerMinute = '';
    maxConcurrency = '';
    expiresAt = '';
    errors = {};
  }

  function edit(key: ApiKey) {
    editing = key;
    name = key.name;
    scopes = [...key.scopes];
    allowedRoutes = [...key.allowed_routes];
    requestsPerMinute = key.requests_per_minute?.toString() ?? '';
    tokensPerMinute = key.tokens_per_minute?.toString() ?? '';
    maxConcurrency = key.max_concurrency?.toString() ?? '';
    expiresAt = key.expires_at ? dateTimeLocalValue(key.expires_at) : '';
    errors = {};
    errorMessage = '';
    notice = '';
  }

  function cancelEdit() {
    editing = null;
    resetForm();
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    errors = validateApiKey({
      name,
      requestsPerMinute: numberValue(requestsPerMinute),
      tokensPerMinute: numberValue(tokensPerMinute),
      maxConcurrency: numberValue(maxConcurrency)
    });
    if (Object.keys(errors).length) return;
    if (!scopes.length) { errorMessage = 'Select at least one scope.'; return; }
    busy = editing ? 'update' : 'create'; errorMessage = ''; notice = '';
    try {
      const input = {
        name: name.trim(),
        scopes,
        allowed_routes: allowedRoutes,
        requests_per_minute: numberValue(requestsPerMinute) ?? null,
        tokens_per_minute: numberValue(tokensPerMinute) ?? null,
        max_concurrency: numberValue(maxConcurrency) ?? null,
        expires_at: expiresAt ? new Date(expiresAt).toISOString() : null
      };
      if (editing) {
        const keyName = editing.name;
        await updateApiKey(editing, input);
        editing = null;
        resetForm();
        await keys.refetch();
        notice = `${keyName} policy updated. Gateways will converge on the new runtime generation.`;
      } else {
        secret = await createApiKey(input);
        secretContext = 'created';
      }
      await queryClient.invalidateQueries({ queryKey: ['api-keys'] });
      await queryClient.invalidateQueries({ queryKey: ['api-key-page'] });
    } catch (error) { errorMessage = message(error); } finally { busy = ''; }
  }

  async function rotate(key: ApiKey) {
    if (!confirm(`Rotate “${key.name}”? Existing clients stop authenticating when revocation converges.`)) return;
    busy = `rotate-${key.id}`; errorMessage = ''; notice = '';
    try {
      secret = await rotateApiKey(key);
      secretContext = 'rotated';
      await keys.refetch();
    } catch (error) { errorMessage = message(error); } finally { busy = ''; }
  }

  async function revoke(key: ApiKey) {
    if (!confirm(`Revoke “${key.name}”? This cannot be undone.`)) return;
    busy = `revoke-${key.id}`; errorMessage = ''; notice = '';
    try { await revokeApiKey(key); await keys.refetch(); }
    catch (error) { errorMessage = message(error); }
    finally { busy = ''; }
  }

  async function copy(value: string, label: string) {
    await navigator.clipboard.writeText(value);
    copied = label;
    setTimeout(() => { if (copied === label) copied = ''; }, 1800);
  }

  function selectSdk(option: typeof sdk) {
    sdk = option;
    testState = 'idle';
    testMessage = '';
  }

  function moveSdkTab(event: KeyboardEvent, index: number) {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === 'Home'
      ? 0
      : event.key === 'End'
        ? sdkOptions.length - 1
        : (index + (event.key === 'ArrowRight' ? 1 : -1) + sdkOptions.length) % sdkOptions.length;
    selectSdk(sdkOptions[next]);
    requestAnimationFrame(() => document.getElementById(`sdk-tab-${sdkOptions[next]}`)?.focus());
  }

  async function testGeneratedKey() {
    if (!secret) return;
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
          body: JSON.stringify({ model: routeSlug, max_tokens: 16, messages: [{ role: 'user', content: 'Connection test' }] })
        });
      } else if (sdk === 'gemini') {
        response = await fetch(`${endpoint}/gemini/v1beta/models/${encodeURIComponent(routeSlug)}:generateContent`, {
          method: 'POST',
          headers: { 'content-type': 'application/json', 'x-goog-api-key': secret.secret },
          body: JSON.stringify({ contents: [{ role: 'user', parts: [{ text: 'Connection test' }] }] })
        });
      } else {
        response = await fetch(`${endpoint}/openai/v1/responses`, {
          method: 'POST',
          headers: { 'content-type': 'application/json', authorization: `Bearer ${secret.secret}` },
          body: JSON.stringify({ model: routeSlug, input: 'Connection test', max_output_tokens: 16 })
        });
      }
      if (!response.ok) {
        let detail = `Request failed (${response.status}).`;
        try {
          const problem = await response.json() as { detail?: string; error?: { message?: string } };
          detail = problem.detail ?? problem.error?.message ?? detail;
        } catch {
          // The status remains enough when an intermediary returns no JSON.
        }
        throw new Error(detail);
      }
      await response.body?.cancel();
      testState = 'passed';
      testMessage = `${sdk === 'openai' ? 'OpenAI' : sdk === 'anthropic' ? 'Anthropic' : 'Gemini'} request succeeded through route ${routeSlug}.`;
    } catch (error) {
      testState = 'failed';
      testMessage = error instanceof Error ? error.message : 'The generated key test failed.';
    }
  }

  function dismissSecret() {
    secret = null;
    copied = '';
    testState = 'idle';
    testMessage = '';
    if (isNew) void goto(resolve('/api-keys'));
  }

  function nextKeyPage() {
    const next = keys.data?.nextCursor;
    if (!next) return;
    listState.history = [...listState.history, listState.cursor];
    listState.cursor = next;
  }

  function previousKeyPage() {
    listState.cursor = listState.history.at(-1);
    listState.history = listState.history.slice(0, -1);
  }
</script>

<svelte:head><title>API Keys · OpenLLMProxy</title></svelte:head>

{#if secret}
  <SecretDialog eyebrow={secretContext === 'created' ? 'Key created' : 'Key rotated'} title="Copy this secret now." description="The full proxy key is shown once. It cannot be retrieved after this panel closes." size="wide" onClose={dismissSecret}>
    {#snippet children(close)}
      <span class="secret-icon" aria-hidden="true"><NavIcon name="key" size={26} /></span>
      <div class="secret-row"><code>{secret!.secret}</code><button class="button button-secondary" type="button" onclick={() => copy(secret!.secret, 'secret')}>{copied === 'secret' ? 'Copied' : 'Copy key'}</button></div>
      <div class="snippet-heading"><div><strong>Test with a vendor SDK</strong><small>Route slugs are sent as the model.</small></div><div class="tabs" role="tablist" aria-label="SDK language">{#each sdkOptions as option, index (option)}<button id={`sdk-tab-${option}`} class:active={sdk === option} role="tab" aria-selected={sdk === option} aria-controls={`sdk-panel-${option}`} tabindex={sdk === option ? 0 : -1} type="button" onclick={() => selectSdk(option)} onkeydown={(event) => moveSdkTab(event, index)}>{option === 'openai' ? 'OpenAI Python' : option === 'anthropic' ? 'Anthropic TS' : 'Gemini TS'}</button>{/each}</div></div>
      <div id={`sdk-panel-${sdk}`} role="tabpanel" aria-labelledby={`sdk-tab-${sdk}`}>
        <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
        <pre tabindex="0"><code>{snippet}</code></pre>
      </div>
      {#if testMessage}<div class:success={testState === 'passed'} class:danger={testState === 'failed'} class="key-test-result" role={testState === 'failed' ? 'alert' : 'status'}>{testMessage}</div>{/if}
      <div class="secret-actions"><button class="button button-secondary" type="button" onclick={() => copy(snippet, 'snippet')}>{copied === 'snippet' ? 'Snippet copied' : 'Copy snippet'}</button><button class="button button-secondary" type="button" onclick={testGeneratedKey} disabled={testState === 'running'}>{testState === 'running' ? 'Testing…' : 'Run connection test'}</button><button class="button button-primary" type="button" data-autofocus onclick={close}>I have saved the key</button></div>
    {/snippet}
  </SecretDialog>
{/if}

{#if isForm}
  <div class="page-header"><div><p class="eyebrow">Access · API Keys</p><h1 class="page-title">{editing ? 'Edit key policy.' : 'Create a proxy key.'}</h1><p class="page-description">{editing ? 'Update scopes, route access, expiry, and shared hard limits. The key secret does not change.' : 'Scope access, restrict route slugs, and apply shared hard limits. The secret is displayed once.'}</p></div>{#if editing}<button class="button button-secondary" type="button" onclick={cancelEdit}>Cancel</button>{:else}<a class="button button-secondary" href={resolve('/api-keys')}>Cancel</a>{/if}</div>
  {#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
  <form class="card key-form" onsubmit={submit} novalidate>
    <section aria-labelledby="identity-heading"><p class="eyebrow">Identity</p><h2 id="identity-heading">Name and expiration</h2><div class="form-grid"><div class="form-field"><label for="key-name">Key name</label><input id="key-name" bind:value={name} aria-invalid={errors.name ? 'true' : undefined} aria-describedby={errors.name ? 'key-name-error' : undefined} />{#if errors.name}<small class="field-error" id="key-name-error">{errors.name}</small>{/if}</div><div class="form-field"><label for="key-expiry">Expires at (optional)</label><input id="key-expiry" type="datetime-local" bind:value={expiresAt} /></div></div></section>
    <section aria-labelledby="scope-heading"><p class="eyebrow">Authorization</p><h2 id="scope-heading">Scopes and route allowlist</h2><fieldset class="checks"><legend>Scopes</legend>{#each [['inference', 'Inference requests'], ['models_read', 'Model listing']] as scope (scope[0])}<label><input type="checkbox" checked={scopes.includes(scope[0])} onchange={(event) => scopes = toggle(scopes, scope[0], event.currentTarget.checked)} /> {scope[1]}</label>{/each}</fieldset><fieldset class="checks routes"><legend>Allowed route slugs</legend><p>Leave every route unchecked to allow all current and future routes.</p>{#if routes.isPending}<span role="status">Loading routes…</span>{:else}{#each routes.data ?? [] as route (route.id)}<label><input type="checkbox" checked={allowedRoutes.includes(route.slug)} onchange={(event) => allowedRoutes = toggle(allowedRoutes, route.slug, event.currentTarget.checked)} /> <code>{route.slug}</code></label>{/each}{#if !routes.data?.length}<span>No routes are configured yet.</span>{/if}{/if}</fieldset></section>
    <section aria-labelledby="limits-heading"><p class="eyebrow">Distributed limits</p><h2 id="limits-heading">Hard runtime limits</h2><p class="section-help">Configured limits fail closed if Valkey is unavailable. Leave blank for no limit.</p><div class="form-grid limits"><div class="form-field"><label for="rpm">Requests per minute</label><input id="rpm" type="number" min="1" inputmode="numeric" bind:value={requestsPerMinute} aria-invalid={errors.requestsPerMinute ? 'true' : undefined} />{#if errors.requestsPerMinute}<small class="field-error">{errors.requestsPerMinute}</small>{/if}</div><div class="form-field"><label for="tpm">Tokens per minute</label><input id="tpm" type="number" min="1" inputmode="numeric" bind:value={tokensPerMinute} aria-invalid={errors.tokensPerMinute ? 'true' : undefined} />{#if errors.tokensPerMinute}<small class="field-error">{errors.tokensPerMinute}</small>{/if}</div><div class="form-field"><label for="concurrency">Concurrent requests</label><input id="concurrency" type="number" min="1" inputmode="numeric" bind:value={maxConcurrency} aria-invalid={errors.maxConcurrency ? 'true' : undefined} />{#if errors.maxConcurrency}<small class="field-error">{errors.maxConcurrency}</small>{/if}</div></div></section>
    <div class="form-actions"><button class="button button-primary" type="submit" disabled={Boolean(busy)}>{busy === 'create' ? 'Creating securely…' : busy === 'update' ? 'Publishing policy…' : editing ? 'Save and publish' : 'Create and show key'} <NavIcon name="arrow" /></button></div>
  </form>
{:else}
  <div class="page-header"><div><p class="eyebrow">Access</p><h1 class="page-title">API Keys</h1><p class="page-description">Issue independent 32-byte proxy keys with scopes, route allowlists, and distributed hard limits.</p></div><a class="button button-primary" href={resolve('/api-keys/new')}>Create key <NavIcon name="arrow" /></a></div>
  {#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
  {#if notice}<div class="success-message" role="status">{notice}</div>{/if}
  {#if keys.isPending}<div class="loading-state" role="status">Loading API keys…</div>
  {:else if keys.isError}<div class="inline-problem" role="alert">{message(keys.error)} <button class="button button-secondary" type="button" onclick={() => keys.refetch()}>Retry</button></div>
  {:else if !keys.data?.items.length && listState.history.length === 0}<section class="card empty-state"><div><h2>No API keys</h2><p>Create a scoped key after activating your first route.</p><a class="button button-primary" href={resolve('/api-keys/new')}>Create first key</a></div></section>
  {:else}<div class="table-shell key-table"><table class="data-table"><thead><tr><th>Name / lookup ID</th><th>Status</th><th>Scope</th><th>Limits</th><th>Creator / created</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each keys.data?.items ?? [] as key (key.id)}<tr><td><strong>{key.name}</strong><br /><code>{key.lookup_id}</code></td><td><span class:danger={Boolean(key.revoked_at)} class:warning={Boolean(key.expires_at && new Date(key.expires_at) < new Date())} class:success={!key.revoked_at && (!key.expires_at || new Date(key.expires_at) >= new Date())} class="badge">{key.revoked_at ? 'revoked' : key.expires_at && new Date(key.expires_at) < new Date() ? 'expired' : 'active'}</span></td><td>{key.scopes.join(', ') || 'none'}<br /><small>{key.allowed_routes.length ? key.allowed_routes.join(', ') : 'all routes'}</small></td><td><small>{key.requests_per_minute ? `${key.requests_per_minute} RPM` : 'unlimited RPM'}<br />{key.tokens_per_minute ? `${key.tokens_per_minute} TPM` : 'unlimited TPM'} · {key.max_concurrency ? `${key.max_concurrency} concurrent` : 'unlimited concurrency'}</small></td><td><strong>{key.created_by_email}</strong><br /><small>{new Date(key.created_at).toLocaleDateString()}</small></td><td><div class="row-actions">{#if !key.revoked_at && (!key.expires_at || new Date(key.expires_at) >= new Date())}<button class="button button-secondary" type="button" onclick={() => edit(key)} disabled={Boolean(busy)}>Edit</button><button class="button button-secondary" type="button" onclick={() => rotate(key)} disabled={Boolean(busy)}>{busy === `rotate-${key.id}` ? 'Rotating…' : 'Rotate'}</button><button class="button button-secondary danger-button" type="button" onclick={() => revoke(key)} disabled={Boolean(busy)}>{busy === `revoke-${key.id}` ? 'Revoking…' : 'Revoke'}</button>{/if}</div></td></tr>{/each}</tbody></table></div><CursorPagination page={listState.history.length + 1} hasPrevious={listState.history.length > 0} hasNext={Boolean(keys.data?.nextCursor)} onPrevious={previousKeyPage} onNext={nextKeyPage} label="API key pages" />{/if}
{/if}

<style>
  h2 { margin: 0 0 .75rem; font-size: 1.15rem; letter-spacing: -.025em; }
  .key-form { display: grid; max-width: 66rem; gap: 2rem; margin-top: 1.5rem; padding: clamp(1.2rem, 3vw, 2rem); }
  .key-form section + section { padding-top: 1.5rem; border-top: 1px solid var(--border); }
  .checks { display: flex; flex-wrap: wrap; gap: .65rem 1rem; margin: 0; padding: 0; border: 0; }
  .checks legend { width: 100%; margin-bottom: .4rem; font-weight: 700; }
  .checks label { display: inline-flex; min-height: 2.75rem; align-items: center; gap: .45rem; }
  .checks.routes { display: grid; margin-top: 1rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; }
  .checks.routes p, .section-help { margin: 0; color: var(--foreground-muted); font-size: .8rem; }
  .limits { grid-template-columns: repeat(3, 1fr); }
  .field-error { color: var(--danger) !important; font-weight: 700; }
  .success-message { margin: 1rem 0; padding: .8rem 1rem; border-radius: .375rem; background: var(--success-soft); color: var(--success); font-weight: 700; }
  .form-actions { display: flex; justify-content: flex-end; }
  .key-table { margin-top: 1.5rem; }
  code { font: .72rem 'JetBrains Mono Variable', monospace; }
  td small { color: var(--foreground-muted); }
  .row-actions { display: flex; gap: .4rem; } .danger-button { color: var(--danger); }
  .secret-icon { display: grid; width: 2.75rem; height: 2.75rem; place-items: center; margin-bottom: 1rem; border-radius: .375rem; background: var(--success-soft); color: var(--success); }
  .secret-row { display: flex; align-items: stretch; margin: 1rem 0; overflow: hidden; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface-subtle); }
  .secret-row code { min-width: 0; flex: 1; overflow-x: auto; padding: .8rem; font-size: .82rem; }
  .secret-row .button { border-width: 0 0 0 1px; border-radius: 0; }
  .snippet-heading { display: flex; align-items: flex-end; justify-content: space-between; gap: 1rem; margin-top: 1.5rem; }
  .snippet-heading > div:first-child { display: grid; } .snippet-heading small { color: var(--foreground-muted); }
  .tabs { display: flex; gap: .25rem; }
  .tabs button { min-height: 2.5rem; padding: .5rem .65rem; border: 0; border-radius: .375rem; background: transparent; color: var(--foreground-muted); }
  .tabs button.active { background: var(--accent-soft); color: var(--accent-strong); font-weight: 700; }
  pre { max-height: 18rem; overflow: auto; padding: 1rem; border-radius: .375rem; background: var(--code-bg); color: var(--code-foreground); }
  pre code { font-size: .76rem; white-space: pre; }
  .key-test-result { margin-top: .65rem; padding: .7rem .8rem; border: 1px solid currentColor; border-radius: .375rem; font-size: .78rem; }
  .key-test-result.success { color: var(--success); background: var(--success-soft); }
  .key-test-result.danger { color: var(--danger); background: var(--danger-soft); }
  .secret-actions { display: flex; justify-content: flex-end; gap: .65rem; }
  @media (forced-colors: active) { pre { border: 1px solid CanvasText; background: Canvas; color: CanvasText; } }
  @media (max-width: 48rem) { .limits { grid-template-columns: 1fr; } .snippet-heading { display: grid; } .tabs { overflow-x: auto; } .secret-row { display: grid; } .secret-row .button { border-width: 1px 0 0; } .secret-actions { display: grid; } }
</style>
