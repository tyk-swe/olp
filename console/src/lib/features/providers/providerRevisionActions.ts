import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import type { Provider } from '#lib/domain/providers/api.js';
import { diffProviderRevisions, restoreProviderRevision } from './revisions';
import { activationReceipt, type ProviderWrite } from './providerWrite';

export async function restoreRevision(
  write: ProviderWrite,
  intent: MutationIntent,
  target: { readonly revisionId: string; readonly revision: number }
) {
  const current = write.writable();
  if (!current) return false;
  return write.run('revision-restore', async (isCurrent) => {
    const submission = intent.prepare(
      { providerId: current.id, revisionId: target.revisionId },
      () => ({
        provider: { id: current.id, etag: current.etag },
        revisionId: target.revisionId
      })
    );
    await restoreProviderRevision(
      submission.input.provider,
      submission.input.revisionId,
      submission.requestId
    );
    if (!isCurrent()) return;
    intent.reset();
    write.resetModelPage();
    await write.observe(
      activationReceipt(
        current.id,
        `Revision ${target.revision} restored as a new draft. No historical credential was restored; the current credential selection was preserved. Test and certify before activation.`
      ),
      isCurrent
    );
    return true;
  });
}

export async function compareRevisions(
  write: ProviderWrite,
  current: Provider | undefined,
  from: string,
  to: string
) {
  if (!current) return;
  return write.run('revision-diff', () =>
    diffProviderRevisions(current.id, from, to)
  );
}
