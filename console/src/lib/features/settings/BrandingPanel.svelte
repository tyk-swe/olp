<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { getBranding, updateBranding, brandingKey } from './api/branding';
  import { authLifecycle } from '$lib/features/access/session/lifecycle';
  import { useRole } from '$lib/features/access/session/useRole.svelte';

  const access = useRole();
  const queryClient = useQueryClient();
  const identity = createQuery(() => ({
    queryKey: brandingKey,
    queryFn: getBranding
  }));
  let name = $state('');
  let logo = $state('');
  let etag = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  $effect(() => {
    if (identity.data && !etag) {
      name = identity.data.name;
      logo = identity.data.logo;
      etag = identity.data.etag;
    }
  });

  async function chooseLogo(file?: File) {
    error = notice = '';
    if (!file) return;
    if (!['image/png', 'image/jpeg'].includes(file.type) || file.size > 65536) {
      error = 'Choose a PNG or JPEG logo of at most 64 KiB.';
      return;
    }
    try {
      busy = true;
      logo = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = () => reject(new Error('The logo could not be read.'));
        reader.readAsDataURL(file);
      });
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }

  async function reload() {
    const result = await identity.refetch();
    if (result.data) {
      name = result.data.name;
      logo = result.data.logo;
      etag = result.data.etag;
      error = notice = '';
    }
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = notice = '';
    try {
      const updated = await updateBranding({ name, logo, etag });
      queryClient.setQueryData(brandingKey, updated);
      name = updated.name;
      logo = updated.logo;
      etag = updated.etag;
      await authLifecycle.validateSession();
      notice = 'Installation branding saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section class="panel branding-panel" aria-labelledby="branding-title">
  <h2 id="branding-title">Installation identity</h2>
  <p class="helper">
    Name this installation and use an embedded logo in the console. Images stay
    in this installation; the console keeps its dark theme and accessible text.
  </p>
  {#if identity.isPending}<p role="status">
      Loading installation identity…
    </p>{/if}
  {#if identity.isError}<p class="inline-problem" role="alert">
      {errorMessage(identity.error)}
    </p>{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if notice}<p class="success-banner" role="status">{notice}</p>{/if}
  <form onsubmit={save}>
    <label
      >Installation name<input
        class="filter-control"
        bind:value={name}
        maxlength="100"
        required
        disabled={!access.can('settings.update') || busy || !etag}
      /></label
    >
    <label
      >Logo<input
        type="file"
        accept="image/png,image/jpeg"
        onchange={(event) => chooseLogo(event.currentTarget.files?.[0])}
        disabled={!access.can('settings.update') || busy || !etag}
      /></label
    >
    <p class="helper">
      PNG or JPEG, at most 64 KiB and 512 × 512 pixels. Leave the logo empty to
      use the default brand mark.
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
        >Use default logo</button
      >
      <button
        class="button button-secondary"
        type="button"
        onclick={reload}
        disabled={busy}>Reload identity</button
      >
      {#if access.can('settings.update')}<button
          class="button button-primary"
          disabled={busy || !etag}>{busy ? 'Saving…' : 'Save identity'}</button
        >{/if}
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
  .button-row {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
</style>
