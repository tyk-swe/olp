<script lang="ts">
  import PageHeader from '#lib/components/PageHeader.svelte';
  import Tabs from '#lib/components/Tabs.svelte';
  import { page } from '$app/state';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { deleteProvider } from './providers';
  import ProviderBulkModels from './ProviderBulkModels.svelte';
  import ProviderCredentialPool from './ProviderCredentialPool.svelte';
  import { focusErrorSummary } from '#lib/editing/focusError.js';
  import { providerKeys } from './providerKeys';
  import { guardUnsavedChanges } from '#lib/editing/unsavedChanges.js';
  import ConflictNotice from '#lib/shell/ConflictNotice.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import { errorMessage as providerDetailError } from '#lib/management/http.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import ProviderConfigurationSection from './ProviderConfigurationSection.svelte';
  import ProviderCredentialsSection from './ProviderCredentialsSection.svelte';
  import BillingImportSection from './BillingImportSection.svelte';
  import ProviderModelsSection from './ProviderModelsSection.svelte';
  import ProviderRevisionsSection from './ProviderRevisionsSection.svelte';
  import ProviderValidationIssues from './ProviderValidationIssues.svelte';
  import { requiresGrant } from './providerEditor';
  import { ProviderEditor } from './providerEditor.svelte';
  let { providerId }: { providerId: string } = $props();
  const editor = new ProviderEditor(() => providerId);
  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(editor.canManage);
  const snapshot = editor.snapshot;
  const providerKinds = editor.providerKinds;
  const providerSpec = $derived(editor.providerSpec);
  const editorView = $derived(editor.view);
  const busy = $derived(editorView.busy);
  const VIEWS = [
    { key: 'settings', label: 'Settings' },
    { key: 'models', label: 'Models' },
    { key: 'revisions', label: 'Revisions' }
  ] as const;
  const view = $derived(
    VIEWS.find((item) => item.key === page.url.searchParams.get('view'))?.key ??
      'settings'
  );
  const tabs = $derived(
    VIEWS.map((item) => ({
      label: item.label,
      href:
        item.key === 'settings'
          ? resolve(`providers/${providerId}`)
          : `${resolve(`providers/${providerId}`)}?view=${item.key}`
    }))
  );
  guardUnsavedChanges(() => editorView.dirty);
</script>

<PageHeader
  title={snapshot.data?.provider?.name ?? 'Provider'}
  description={snapshot.data?.provider
    ? `Added by ${snapshot.data.provider.created_by_email ?? 'a removed account'}`
    : undefined}
>
  {#snippet actions()}
    {#if canManage && snapshot.data?.provider}<a
        class="button button-secondary"
        href={`${resolve('providers/new')}?copy=${snapshot.data.provider.id}`}
        >Duplicate</a
      >{/if}
  {/snippet}
</PageHeader>
<Tabs
  label="Provider"
  {tabs}
  active={tabs[VIEWS.findIndex((item) => item.key === view)]?.href}
/>
{#if editorView.refreshError}
  <div class="inline-problem" role="alert">
    <p>
      The provider change succeeded, but refreshing failed. {editorView.refreshError}
    </p>
    <button
      class="button button-secondary"
      type="button"
      disabled={Boolean(busy)}
      onclick={editor.retryRefresh}>Retry refresh</button
    >
  </div>
{/if}

{#if editorView.error}<div
    class="inline-problem"
    role="alert"
    tabindex="-1"
    use:focusErrorSummary
  >
    {editorView.error}
    <ProviderValidationIssues issues={editorView.validationIssues} />
  </div>{/if}
{#if editorView.notice}<div class="success-banner" role="status">
    {editorView.notice}
  </div>{/if}
<ConflictNotice
  notice={editorView.concurrentNotice}
  onReload={editor.reload}
  disabled={Boolean(busy)}
/>

{#if providerKinds.isError}
  <div class="inline-problem" role="alert">
    Provider capabilities could not be loaded. Configuration editing is
    unavailable until a retry succeeds. <button
      class="button button-secondary"
      type="button"
      onclick={() => providerKinds.refetch()}>Retry</button
    >
  </div>
{/if}
{#if snapshot.isError && snapshot.data?.provider && !editorView.refreshError}
  <div class="inline-problem" role="alert">
    {providerDetailError(snapshot.error)} The last loaded provider remains available
    below.
    <button
      class="button button-secondary"
      type="button"
      onclick={() => snapshot.refetch()}>Retry</button
    >
  </div>
{/if}

{#if providerKinds.isPending || (snapshot.isPending && !snapshot.data) || (snapshot.data && snapshot.data.provider.id !== providerId)}
  <div class="loading-state" role="status">Loading provider…</div>
{:else if !snapshot.data?.provider}
  <div class="inline-problem" role="alert">
    {providerDetailError(snapshot.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => snapshot.refetch()}>Retry</button
    >
  </div>
{:else}
  {@const current = snapshot.data?.provider}
  {#key editorView.explicitReloadEpoch}
    {#if !canManage}<ReadOnlyNote>
        Your role can view this provider but not change, test, or activate it.
      </ReadOnlyNote>{/if}
    {#if current.pending_activation && current.active_revision != null}<div
        class="pending-banner"
        role="status"
      >
        <strong>Revision {current.active_revision} remains live.</strong><span
          >Draft configuration and the draft-selected credential are not serving
          traffic. Test, certify, and activate to replace the runtime revision
          atomically.</span
        >
      </div>{/if}
    {#if view === 'settings'}
      <div class="detail-grid">
        <ProviderConfigurationSection {current} {providerSpec} {editor} />
        <ProviderCredentialsSection {current} {providerSpec} {editor} />
        {#key current.id}
          <BillingImportSection provider={current} {canManage} />
        {/key}
      </div>
      <ProviderCredentialPool
        provider={current}
        {canManage}
        grant={Boolean(
          providerSpec &&
          requiresGrant(providerSpec, current.configuration.auth_mode)
        )}
        onChanged={editor.refresh}
      />
      {#if current.state === 'draft' && !current.active_revision && access.allows('DELETE /api/v1/providers/{provider_id}')}<ConfirmAction
          danger
          label="Delete unused provider"
          description={`Delete ${current.name} and its draft credentials. Providers referenced by routes or history cannot be deleted`}
          identity={`${current.id}:${current.etag}`}
          disabled={Boolean(busy)}
          run={(requestId) => deleteProvider(current, requestId)}
          done={async () => {
            await queryClient.invalidateQueries({
              queryKey: providerKeys.summaries
            });
            await goto(resolve('providers'));
          }}
        />{/if}
    {:else if view === 'models'}
      <ProviderBulkModels
        provider={current}
        {canManage}
        onChanged={editor.refresh}
      />
      <ProviderModelsSection {current} {editor} />
    {:else}
      <ProviderRevisionsSection {current} {editor} />
    {/if}
  {/key}
{/if}

<style>
  .pending-banner {
    display: grid;
    gap: 0.2rem;
    margin: 1rem 0;
    padding: 0.9rem 1rem;
    border: 1px solid var(--warning);
    border-radius: var(--radius-control);
    background: var(--warning-soft);
    color: var(--foreground);
  }
  .pending-banner strong {
    color: var(--warning);
  }
  .pending-banner span {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .detail-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.35fr) minmax(18rem, 0.65fr);
    gap: 1rem;
  }
  @media (max-width: 64rem) {
    .detail-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
