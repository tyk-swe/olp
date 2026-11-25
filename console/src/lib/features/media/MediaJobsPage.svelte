<script lang="ts">
  import MediaJobDetail from './MediaJobDetail.svelte';
  import MediaJobList from './MediaJobList.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { mediaJobKeys } from '#lib/features/media/mediaJobKeys.js';

  import { resolve } from '$app/paths';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { applyListSearch } from '#lib/listing/urlSync.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    deleteMediaJob,
    downloadMediaJobContent,
    getMediaJob,
    listMediaJobs,
    refreshMediaJob,
    type MediaContentVariant
  } from '#lib/features/media/api.js';
  import { errorMessage } from '#lib/management/http.js';
  import { downloadBlob } from '#lib/download.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    mediaJobFilters,
    mediaJobPending,
    mediaJobPollInterval,
    mediaJobProblem,
    mediaJobSearch,
    mediaJobUrl,
    readMediaJobForm,
    type MediaJobListState
  } from '#lib/features/media/mediaJobListState.js';

  let {
    jobId = '',
    listState = $bindable()
  }: {
    jobId?: string;
    listState: MediaJobListState;
  } = $props();

  let validation = $state<string | null>(null);
  const urlForm = $derived(readMediaJobForm(page.url.searchParams));
  const urlFilters = $derived(mediaJobFilters(urlForm));
  const urlProblem = $derived(mediaJobProblem(urlForm, true));
  $effect(() => {
    void page.url.search;
    validation = null;
  });

  const queryClient = useQueryClient();
  const access = useRole();
  const canOperate = $derived(access.can('media.manage'));

  const jobAction = useAction();
  let downloading = $state(false);
  const actionBusy = $derived(
    jobAction.key || (downloading ? 'download' : null)
  );
  let actionError = $state<string | null>(null);
  let downloadVariant = $state<MediaContentVariant>('video');

  const jobs = createQuery(() => {
    const applied = urlFilters;
    const cursor =
      mediaJobSearch(applied) === mediaJobSearch(listState.applied)
        ? listState.cursor
        : undefined;
    return {
      queryKey: mediaJobKeys.page(applied, cursor),
      queryFn: () => listMediaJobs({ ...applied, cursor }),
      placeholderData: (previous) => previous,
      enabled: !jobId && !urlProblem,
      refetchInterval: (query) =>
        mediaJobPollInterval(
          query.state.data?.items.some(mediaJobPending) ?? false
        )
    };
  });
  const detail = createQuery(() => ({
    queryKey: mediaJobKeys.detail(jobId),
    queryFn: () => getMediaJob(jobId),
    enabled: Boolean(jobId),
    refetchInterval: (query) =>
      mediaJobPollInterval(
        query.state.data != null && mediaJobPending(query.state.data)
      )
  }));

  async function refreshNow() {
    if (actionBusy) return;
    actionError = null;
    await jobAction.run(
      () => refreshMediaJob(jobId),
      (updated) => {
        queryClient.setQueryData(mediaJobKeys.detail(jobId), updated);
        void queryClient.invalidateQueries({ queryKey: mediaJobKeys.all });
      },
      {
        key: 'refresh',
        failure: (error) =>
          errorMessage(error, 'The refresh could not be completed.')
      }
    );
    actionError = jobAction.error;
  }

  async function download() {
    if (actionBusy) return;
    downloading = true;
    actionError = null;
    const result = await readResult(async () => {
      const { blob, filename } = await downloadMediaJobContent(
        jobId,
        downloadVariant
      );
      downloadBlob(blob, filename);
    });
    downloading = false;
    if (!result.ok)
      actionError = errorMessage(
        result.error,
        'The content could not be downloaded.'
      );
  }

  async function remove() {
    const job = detail.data;
    if (!job || actionBusy) return;
    if (
      !(await confirmAction({
        title: 'Delete the provider’s copy of this output?',
        body: 'This only removes the file stored at the provider. It doesn’t cancel the job.',
        confirmLabel: 'Delete output',
        danger: true
      }))
    )
      return;
    actionError = null;
    await jobAction.run(
      () => deleteMediaJob(jobId, job.etag),
      async (_, current) => {
        await queryClient.invalidateQueries({ queryKey: mediaJobKeys.all });
        if (!current()) return;
        await goto(resolve(`media-jobs${page.url.search}`));
      },
      {
        key: 'delete',
        failure: (error) => {
          // A refused delete may still have moved the job (for example to
          // delete_pending with a new ETag), so show the server's current
          // state.
          void queryClient.invalidateQueries({ queryKey: mediaJobKeys.all });
          return errorMessage(error, 'The job could not be deleted.');
        }
      }
    );
    actionError = jobAction.error;
  }

  function apply(event: SubmitEvent) {
    event.preventDefault();
    validation = mediaJobProblem(listState, false, listState.applied);
    if (validation) return;
    applyListSearch(
      listState,
      mediaJobUrl,
      mediaJobSearch(mediaJobFilters(listState, listState.applied))
    );
  }

  function clear() {
    validation = null;
    applyListSearch(listState, mediaJobUrl, '');
  }
</script>

<PageTitle title="Media jobs" />

{#if jobId}
  <PageHeader
    title="Media job"
    description="Lifecycle and reconciliation metadata only. Uploaded and generated media never appears in the console."
  >
    {#snippet actions()}
      <div class="page-actions">
        <a
          class="button button-secondary"
          href={resolve(`media-jobs${page.url.search}`)}
        >
          All media jobs
        </a>
        <button
          class="button button-secondary"
          type="button"
          onclick={() => detail.refetch()}
          disabled={detail.isFetching}
        >
          Refresh
        </button>
      </div>
    {/snippet}
  </PageHeader>
  {#if detail.isPending}<div class="loading-state" role="status">
      Loading media job…
    </div>
  {:else if detail.isError}<div class="inline-problem" role="alert">
      {errorMessage(detail.error, 'The media job could not be loaded.')}
      <button
        class="text-button"
        type="button"
        onclick={() => detail.refetch()}
      >
        Retry
      </button>
    </div>
  {:else if detail.data}
    <MediaJobDetail
      job={detail.data}
      {canOperate}
      {actionBusy}
      {actionError}
      bind:downloadVariant
      onDownload={download}
      onRefresh={refreshNow}
      onRemove={remove}
    />
  {/if}
{:else}
  <PageHeader
    title="Media jobs"
    description="Video and other long-running media requests. Uploaded and generated files are never shown here."
  >
    {#snippet actions()}
      <button
        class="button button-secondary"
        type="button"
        onclick={() => jobs.refetch()}
        disabled={jobs.isFetching || Boolean(urlProblem)}
      >
        Refresh
      </button>
    {/snippet}
  </PageHeader>
  <MediaJobList
    bind:listState
    problem={validation ?? urlProblem}
    blocked={Boolean(urlProblem)}
    pending={jobs.isPending}
    error={jobs.isError
      ? errorMessage(jobs.error, 'Media jobs are unavailable.')
      : null}
    items={jobs.data?.items ?? []}
    hasHistory={listState.history.length > 0}
    placeholder={jobs.isPlaceholderData}
    nextCursor={jobs.data?.nextCursor}
    onApply={apply}
    onClear={clear}
    onRetry={() => jobs.refetch()}
  />
{/if}
