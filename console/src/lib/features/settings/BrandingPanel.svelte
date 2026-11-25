<script lang="ts">
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    brandingKey,
    useBranding,
    type Branding
  } from '#lib/shell/branding/branding.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { updateBranding } from './api.js';

  const defaultAccent = '#2f64ee';
  const access = useRole();
  const queryClient = useQueryClient();
  const identity = useBranding();
  let name = $state('');
  let logo = $state('');
  let accent = $state('');
  let etag = $state('');
  const saveAction = useAction();
  let reading = $state(false);
  const busy = $derived(saveAction.busy || reading);
  let error = $state('');
  const editable = $derived(access.can('settings.update') && !busy && !!etag);

  function load(branding: Branding) {
    name = branding.name;
    logo = branding.logo;
    accent = branding.accent;
    etag = branding.etag;
  }

  $effect(() => {
    if (identity.data && !etag) load(identity.data);
  });

  async function chooseLogo(file?: File) {
    error = '';
    if (!file) return;
    if (!['image/png', 'image/jpeg'].includes(file.type) || file.size > 65536) {
      error = 'Choose a PNG or JPEG logo of at most 64 KiB.';
      return;
    }
    reading = true;
    const result = await readResult(
      () =>
        new Promise<string>((resolve, reject) => {
          const reader = new FileReader();
          reader.onload = () => resolve(String(reader.result));
          reader.onerror = () =>
            reject(new Error('The logo could not be read.'));
          reader.readAsDataURL(file);
        })
    );
    reading = false;
    if (!result.ok) error = errorMessage(result.error);
    else logo = result.value;
  }

  async function reload() {
    const result = await identity.refetch();
    if (result.data) {
      load(result.data);
      error = '';
    }
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    await saveAction.run(
      () => updateBranding({ name, logo, accent }, etag),
      (updated, current) => {
        if (!current()) return;
        queryClient.setQueryData(brandingKey, updated);
        load(updated);
        notify('Installation branding saved.');
      },
      { key: 'save', failure: (cause) => errorMessage(cause) }
    );
    error = saveAction.error;
  }
</script>

<section class="panel branding-panel" aria-labelledby="branding-title">
  <h2 id="branding-title">Installation identity</h2>
  <p class="muted">
    Name this installation, embed a logo and pick a brand color. They appear on
    the sign-in page, in browser tabs and across the console. Images stay in
    this installation.
  </p>
  {#if identity.isPending}<p role="status">
      Loading installation identity…
    </p>{/if}
  {#if identity.isError}<p class="inline-problem" role="alert">
      {errorMessage(identity.error)}
    </p>{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}

  <form onsubmit={save}>
    <label>
      Installation name<input
        class="filter-control"
        bind:value={name}
        maxlength="100"
        required
        disabled={!editable}
      />
    </label>
    <label>
      Logo<input
        type="file"
        accept="image/png,image/jpeg"
        onchange={(event) => chooseLogo(event.currentTarget.files?.[0])}
        disabled={!editable}
      />
    </label>
    <p class="muted">
      PNG or JPEG, at most 64 KiB and 512 × 512 pixels. Leave the logo empty to
      use the default brand mark.
    </p>
    <fieldset class="accent">
      <legend>Brand color</legend>
      <input
        type="color"
        aria-label="Brand color picker"
        value={accent || defaultAccent}
        oninput={(event) => (accent = event.currentTarget.value)}
        disabled={!editable}
      />
      <input
        class="filter-control"
        aria-label="Brand color hex"
        bind:value={accent}
        placeholder="Default"
        pattern={'#[0-9a-fA-F]{6}'}
        maxlength="7"
        disabled={!editable}
      />
      <button
        class="button button-secondary"
        type="button"
        onclick={() => (accent = '')}
        disabled={!accent || !editable}
      >
        Use default color
      </button>
    </fieldset>
    <p class="muted">
      Primary buttons use this color with white text, so it needs at least 4.5:1
      contrast against white. The console derives lighter and darker shades that
      stay readable on every surface.
    </p>
    {#if logo}<img
        class="logo-preview"
        src={logo}
        alt="Installation logo preview"
      />{/if}
    <div class="button-row">
      <button
        class="button button-secondary"
        type="button"
        onclick={() => (logo = '')}
        disabled={!logo || !access.can('settings.update') || busy}
      >
        Use default logo
      </button>
      <button
        class="button button-secondary"
        type="button"
        onclick={reload}
        disabled={busy}
      >
        Reload identity
      </button>
      {#if access.can('settings.update')}<button
          class="button button-primary"
          disabled={busy || !etag}
        >
          {busy ? 'Saving…' : 'Save identity'}
        </button>{/if}
    </div>
  </form>
</section>

<style>
  .branding-panel {
    margin-bottom: 2rem;
  }
  form {
    display: grid;
    gap: 1rem;
    max-width: 40rem;
  }
  label {
    display: grid;
    gap: 0.4rem;
  }
  .logo-preview {
    width: 4rem;
    height: 4rem;
    object-fit: contain;
    border-radius: var(--radius-control);
  }
  .accent {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
    margin: 0;
    padding: 0;
  }
  .accent legend {
    margin-bottom: 0.4rem;
    padding: 0;
  }
  .accent input[type='color'] {
    width: 2.75rem;
    height: 2.75rem;
    padding: 0;
    border-radius: var(--radius-control);
    background: none;
  }
  .accent .filter-control {
    width: 8rem;
  }
  .button-row {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
</style>
