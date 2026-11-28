import type { QueryClient } from '@tanstack/svelte-query';
import { goto } from '$app/navigation';
import { resolve } from '$app/paths';
import { confirmAction } from '#lib/components/confirm.js';
import { routeKeys } from '#lib/domain/routes/routeKeys.js';
import { errorMessage } from '#lib/management/http.js';
import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import {
  initialConcurrentEdit,
  markSaved,
  type ConcurrentEditState
} from '#lib/editing/concurrentEdit.js';
import {
  createRouteDraft,
  deleteRouteDraft,
  replaceRouteDraft,
  type RouteDraft
} from '#lib/features/routes/api.js';
import {
  buildCreateRouteDraftInput,
  buildReplaceRouteDraftInput
} from '#lib/features/routes/routeDraftInput.js';
import type { RouteEditorValues } from '#lib/features/routes/routeEditor.js';
import type { RouteModelOption } from '#lib/features/routes/routeTargetModel.js';

export type RouteDraftPort = {
  readonly queryClient: QueryClient;
  readonly run: (
    label: string,
    action: (
      isCurrent: () => boolean,
      samePrincipal: () => boolean
    ) => Promise<void>
  ) => Promise<void>;
  readonly sync: () => ConcurrentEditState;
  readonly setSync: (next: ConcurrentEditState) => void;
  readonly setNotice: (message: string) => void;
};

export function openCreatedDraft(port: RouteDraftPort, id: string) {
  return port.run('open', async (isCurrent) => {
    if (isCurrent()) await goto(resolve(`routes/${id}`));
  });
}

export function createDraft(
  port: RouteDraftPort,
  submission: {
    readonly intent: MutationIntent;
    readonly values: RouteEditorValues;
    readonly models: RouteModelOption[];
    readonly onCreated: (id: string) => void;
    readonly onRefreshFailed: (message: string) => void;
  }
) {
  return port.run('save', async (isCurrent, samePrincipal) => {
    const { id } = await submission.intent.submit(
      buildCreateRouteDraftInput(submission.values, submission.models),
      (input, requestId) => createRouteDraft(input, requestId)
    );
    if (!samePrincipal()) return;
    if (isCurrent()) {
      submission.onCreated(id);
      port.setSync(initialConcurrentEdit());
      port.setNotice('Draft saved.');
    }
    try {
      await port.queryClient.invalidateQueries(
        { queryKey: routeKeys.lists },
        { throwOnError: true }
      );
    } catch (cause) {
      if (isCurrent())
        submission.onRefreshFailed(
          `Draft saved, but refreshing failed. ${errorMessage(cause)}`
        );
    }
    if (isCurrent()) await goto(resolve(`routes/${id}`));
  });
}

export function saveDraft(
  port: RouteDraftPort,
  current: RouteDraft,
  values: RouteEditorValues,
  editVersion: () => number
) {
  return port.run('save', async (isCurrent, samePrincipal) => {
    const snapshotEtag = port.sync().snapshotEtag;
    if (!snapshotEtag) throw new Error('Reload the draft before saving.');
    const submittedVersion = editVersion();
    const updated = await replaceRouteDraft(
      current.id,
      snapshotEtag,
      buildReplaceRouteDraftInput(values)
    );
    if (!samePrincipal()) return;
    port.queryClient.setQueryData(routeKeys.draft(current.id), updated);
    if (!isCurrent()) return;
    const stillDirty = editVersion() !== submittedVersion;
    port.setSync(markSaved(updated.etag, stillDirty));
    port.setNotice(
      stillDirty
        ? 'Draft saved. You have additional unsaved changes.'
        : 'Draft saved. Validate to preview, or activate directly; activation validates the saved draft.'
    );
  });
}

export async function removeDraft(port: RouteDraftPort, current: RouteDraft) {
  const confirmed = await confirmAction({
    title: `Delete draft ${current.slug}?`,
    body: 'The draft is removed. Live revisions are not affected.',
    confirmLabel: 'Delete draft',
    danger: true
  });
  if (!confirmed) return;
  await port.run('delete', async (isCurrent) => {
    await deleteRouteDraft(current.id, current.etag);
    await port.queryClient.invalidateQueries({ queryKey: routeKeys.lists });
    if (!isCurrent()) return;
    port.setSync(initialConcurrentEdit());
    await goto(resolve('routes'));
  });
}
