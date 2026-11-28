import type { QueryClient } from '@tanstack/svelte-query';
import { errorMessage } from '#lib/management/http.js';
import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import type { ProviderKindCapability } from '#lib/domain/providers/api.js';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import {
  activateProvider,
  disableProvider,
  isProviderInUse,
  probeProvider,
  restoreProviderAsDraft,
  updateProvider
} from './providers';
import type { ProviderModelPage } from './models';
import {
  buildUpdateProviderInput,
  type ProviderEditValues
} from './providerEditor';
import { disableNotice, probeSummary } from './providerReadiness';
import { activationReceipt, type ProviderWrite } from './providerWrite';

export async function saveDraft(
  write: ProviderWrite,
  queryClient: QueryClient,
  draft: {
    readonly spec: ProviderKindCapability | undefined;
    readonly snapshotEtag: string | null;
    readonly values: ProviderEditValues;
    readonly version: () => number;
    readonly onSaved: (etag: string, stillDirty: boolean) => void;
  }
) {
  const current = write.writable();
  const { spec } = draft;
  if (!current || !spec) return false;
  return write.run('save', async (isCurrent) => {
    if (!draft.snapshotEtag)
      throw new Error('Reload the provider before saving.');
    const submittedVersion = draft.version();
    const updated = await updateProvider(
      current.id,
      draft.snapshotEtag,
      buildUpdateProviderInput(draft.values, spec)
    );
    if (!isCurrent()) return;
    const stillDirty = draft.version() !== submittedVersion;
    draft.onSaved(updated.etag, stillDirty);
    queryClient.setQueriesData<ProviderModelPage>(
      { queryKey: providerKeys.modelsOf(current.id) },
      (page) => (page ? { ...page, provider: updated } : page)
    );
    write.resetModelPage();
    await write.observe(
      activationReceipt(
        current.id,
        stillDirty
          ? 'Draft saved. You have additional unsaved changes.'
          : 'Provider draft settings saved.'
      ),
      isCurrent
    );
  });
}

export async function probe(write: ProviderWrite, dirty: boolean) {
  const current = write.writable();
  if (!current || dirty) return false;
  return write.run('detail-probe', async (isCurrent) => {
    const { result } = await probeProvider(current);
    if (!isCurrent()) return;
    if (!result.succeeded) throw new Error(result.detail);
    await write.observe(
      activationReceipt(
        current.id,
        `Connection succeeded: ${probeSummary(result)}`
      ),
      isCurrent
    );
  });
}

export async function activate(
  write: ProviderWrite,
  intent: MutationIntent,
  dirty: boolean
) {
  const current = write.writable();
  if (!current || dirty) return false;
  return write.run('detail-activate', async (isCurrent) => {
    const submission = intent.prepare(current.id, () => ({
      id: current.id,
      etag: current.etag
    }));
    const generation = await activateProvider(
      submission.input,
      submission.requestId
    );
    if (!isCurrent()) return;
    intent.reset();
    await write.observe(
      activationReceipt(
        current.id,
        `Activated in runtime generation ${generation}.`
      ),
      isCurrent
    );
  });
}

export async function disable(
  write: ProviderWrite,
  intent: MutationIntent,
  onReferenceConflict: (message: string) => void
) {
  const current = write.writable();
  if (!current) return false;
  return write.run('detail-disable', async (isCurrent) => {
    const submission = intent.prepare(current.id, () => ({
      id: current.id,
      etag: current.etag
    }));
    try {
      const generation = await disableProvider(
        submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      intent.reset();
      await write.observe(
        activationReceipt(current.id, disableNotice(generation)),
        isCurrent
      );
    } catch (error) {
      if (!isCurrent()) return;
      if (!isProviderInUse(error)) throw error;
      onReferenceConflict(errorMessage(error));
    }
  });
}

export async function restoreAsDraft(
  write: ProviderWrite,
  intent: MutationIntent
) {
  const current = write.writable();
  if (!current) return false;
  return write.run('detail-restore-draft', async (isCurrent) => {
    const submission = intent.prepare(current.id, () => ({
      id: current.id,
      etag: current.etag
    }));
    await restoreProviderAsDraft(submission.input, submission.requestId);
    if (!isCurrent()) return;
    intent.reset();
    await write.observe(
      activationReceipt(
        current.id,
        'Provider restored as a draft. Stored capabilities are declared again: re-certify and test before activation.'
      ),
      isCurrent
    );
  });
}
