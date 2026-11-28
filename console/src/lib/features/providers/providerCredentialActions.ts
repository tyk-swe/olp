import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import type { ProviderCredential } from '#lib/domain/providers/api.js';
import {
  revokeProviderCredential,
  rotateProviderCredential,
  type ExternalCredentialReference
} from './credentials';
import {
  createNetworkCredential,
  revokeNetworkCredential
} from './providerProfiles';
import { activationReceipt, type ProviderWrite } from './providerWrite';

export async function stageCredential(
  write: ProviderWrite,
  intent: MutationIntent,
  credential: string | ExternalCredentialReference
) {
  const current = write.writable();
  if (!current) return false;
  return write.run('rotate-credential', async (isCurrent) => {
    const submission = intent.prepare(
      { providerId: current.id, credential },
      () => [{ id: current.id, etag: current.etag }, credential] as const
    );
    await rotateProviderCredential(...submission.input, submission.requestId);
    if (!isCurrent()) return;
    intent.reset();
    write.resetModelPage();
    await write.observe(
      activationReceipt(
        current.id,
        'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.'
      ),
      isCurrent
    );
    return true;
  });
}

export async function revokeCredential(
  write: ProviderWrite,
  intent: MutationIntent,
  credential: ProviderCredential
) {
  const current = write.writable();
  if (!current) return false;
  return write.run(`revoke-${credential.id}`, async (isCurrent) => {
    const submission = intent.prepare(
      { providerId: current.id, credentialId: credential.id },
      () => [{ id: current.id, etag: current.etag }, credential.id] as const
    );
    await revokeProviderCredential(...submission.input, submission.requestId);
    if (!isCurrent()) return;
    intent.reset();
    await write.observe(
      activationReceipt(
        current.id,
        `Credential version ${credential.version} revoked.`
      ),
      isCurrent
    );
  });
}

export async function stageNetworkCredential(
  write: ProviderWrite,
  intent: MutationIntent,
  secret: string,
  onStored: (credentialId: string) => void
) {
  const current = write.writable();
  if (!current) return;
  return write.run('network-credential', async (isCurrent) => {
    if (!nativeObject(parseNativeJSON(secret)))
      throw new Error('Enter a network credential JSON object.');
    const submission = intent.prepare(
      { providerId: current.id, secret },
      () => [current.id, current.etag, secret] as const
    );
    const created = await createNetworkCredential(
      ...submission.input,
      submission.requestId
    );
    if (!isCurrent()) return;
    intent.reset();
    onStored(created.credential_id);
    await write.observe(
      activationReceipt(
        current.id,
        'Network credential stored and selected in the draft.',
        { previousEtag: submission.input[1], etag: created.etag }
      ),
      isCurrent
    );
    return true;
  });
}

export async function retireNetworkCredential(
  write: ProviderWrite,
  intent: MutationIntent,
  credentialId: string
) {
  const current = write.writable();
  if (!current) return;
  return write.run('revoke-network-credential', async (isCurrent) => {
    const submission = intent.prepare(
      { providerId: current.id, credentialId },
      () => [current.id, current.etag, credentialId] as const
    );
    const revoked = await revokeNetworkCredential(
      ...submission.input,
      submission.requestId
    );
    if (!isCurrent()) return;
    intent.reset();
    await write.observe(
      activationReceipt(current.id, 'Network credential revoked.', {
        previousEtag: submission.input[1],
        etag: revoked.etag
      }),
      isCurrent
    );
  });
}
