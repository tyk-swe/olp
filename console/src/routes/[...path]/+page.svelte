<script lang="ts">
  import { page } from '$app/state';
  import AppShell from '$lib/components/AppShell.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ProvidersPage from '$lib/features/gateway/ProvidersPage.svelte';
  import ModelsPage from '$lib/features/gateway/ModelsPage.svelte';
  import RoutesPage from '$lib/features/gateway/RoutesPage.svelte';
  import ApiKeysPage from '$lib/features/access/ApiKeysPage.svelte';
  import AccessPage from '$lib/features/access/AccessPage.svelte';
  import RequestsPage from '$lib/features/operations/RequestsPage.svelte';
  import MediaJobsPage from '$lib/features/operations/MediaJobsPage.svelte';
  import UsagePage from '$lib/features/operations/UsagePage.svelte';
  import HealthPage from '$lib/features/operations/HealthPage.svelte';
  import AuditPage from '$lib/features/operations/AuditPage.svelte';
  import PlaygroundPage from '$lib/features/playground/PlaygroundPage.svelte';
  import SettingsPage from '$lib/features/settings/SettingsPage.svelte';
  import ProfilePage from '$lib/features/settings/ProfilePage.svelte';

  const path = $derived(page.params.path ?? '');
  const segment = $derived(path.split('/')[0]);
</script>

<AppShell>
  {#if segment === 'providers'}
    <ProvidersPage {path} />
  {:else if segment === 'models'}
    <ModelsPage />
  {:else if segment === 'routes'}
    <RoutesPage {path} />
  {:else if segment === 'api-keys'}
    <ApiKeysPage {path} />
  {:else if segment === 'access'}
    <AccessPage />
  {:else if segment === 'requests'}
    <RequestsPage {path} />
  {:else if segment === 'media-jobs'}
    <MediaJobsPage {path} />
  {:else if segment === 'usage'}
    <UsagePage />
  {:else if segment === 'health'}
    <HealthPage />
  {:else if segment === 'audit'}
    <AuditPage />
  {:else if segment === 'playground'}
    <PlaygroundPage />
  {:else if path === 'settings/profile'}
    <ProfilePage />
  {:else if segment === 'settings'}
    <SettingsPage />
  {:else}
    <p class="eyebrow">Console</p>
    <h1 class="page-title">Page not found</h1>
    <p class="page-description">This console route does not exist.</p>
    <section class="card not-found" aria-labelledby="not-found-title">
      <span aria-hidden="true"><NavIcon name="request" size={24} /></span>
      <div><h2 id="not-found-title">Return to the overview</h2><p>Check the address or use the primary navigation to continue.</p><a class="button button-secondary" href="/">Go to overview</a></div>
    </section>
  {/if}
</AppShell>

<style>
  .not-found { display: flex; max-width: 48rem; gap: 1rem; margin-top: 2rem; padding: 1.5rem; }
  .not-found > span { display: grid; width: 2.75rem; height: 2.75rem; flex: none; place-items: center; border-radius: 0.375rem; background: var(--accent-soft); color: var(--accent-strong); }
  .not-found h2 { margin: 0; font-size: 1.2rem; }
  .not-found p { margin: 0.5rem 0 1rem; color: var(--foreground-muted); }
</style>
