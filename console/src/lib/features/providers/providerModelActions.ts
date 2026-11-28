import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import type { Provider } from '#lib/domain/providers/api.js';
import {
  certifyProviderModel,
  compareProviderListing,
  declareProviderModels,
  discoverProviderModels,
  setProviderModel,
  type CapabilityDeclaration
} from './models';
import { acceptCatalogSuggestions } from './catalog';
import { probeProvider } from './providers';
import { parseManualModelNames } from './providerEditor';
import { certificationPrerequisiteReady } from './providerReadiness';
import { activationReceipt, type ProviderWrite } from './providerWrite';

export async function compareListing(write: ProviderWrite) {
  const current = write.writable();
  if (!current) return;
  return write.run('listing-comparison', () =>
    compareProviderListing(current.id)
  );
}

export async function discoverModels(write: ProviderWrite) {
  const current = write.writable();
  if (!current) return;
  return write.run('detail-discover', async (isCurrent) => {
    const updated = await discoverProviderModels(current);
    if (!isCurrent()) return;
    write.resetModelPage();
    await write.observe(
      activationReceipt(
        current.id,
        `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} discovered. Review capabilities before activation.`
      ),
      isCurrent
    );
    return true;
  });
}

export async function declareModels(
  write: ProviderWrite,
  input: string,
  onInvalid: (message: string) => void
) {
  const current = write.writable();
  if (!current) return;
  const names = parseManualModelNames(input);
  if (!names.length) {
    onInvalid('Enter at least one upstream model identifier.');
    return;
  }
  return write.run('detail-declare', async (isCurrent) => {
    const updated = await declareProviderModels(current, names);
    if (!isCurrent()) return;
    write.resetModelPage();
    await write.observe(
      activationReceipt(
        current.id,
        `${updated.model_count} manually declared model${updated.model_count === 1 ? '' : 's'} ready for capability review.`
      ),
      isCurrent
    );
    return true;
  });
}

export async function reviewModel(
  write: ProviderWrite,
  provider: Provider,
  review: {
    readonly modelId: string;
    readonly enabled: boolean;
    readonly capabilities: CapabilityDeclaration[];
  }
) {
  if (!write.canWrite(provider)) return false;
  return write.run(`model-${review.modelId}`, async (isCurrent) => {
    await setProviderModel(
      provider,
      review.modelId,
      review.enabled,
      review.capabilities
    );
    if (!isCurrent()) return false;
    await write.observe(
      activationReceipt(
        provider.id,
        'Capability review saved with declared provenance.'
      ),
      isCurrent
    );
    return true;
  });
}

export async function certifyModel(
  write: ProviderWrite,
  provider: Provider,
  modelId: string
) {
  if (!write.canWrite(provider)) return;
  return write.run(`certify-${modelId}`, async (isCurrent) => {
    const probe = certificationPrerequisiteReady(provider)
      ? undefined
      : (await probeProvider(provider)).result;
    if (!isCurrent()) return;
    if (probe && !probe.succeeded) throw new Error(probe.detail);
    const tested = probe
      ? {
          ...provider,
          last_probe_at: probe.checked_at,
          last_probe_status: 'succeeded',
          last_probe_detail: probe.detail
        }
      : provider;
    const { result } = await certifyProviderModel(tested, modelId);
    if (!isCurrent()) return;
    await write.observe(
      activationReceipt(
        provider.id,
        `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
      ),
      isCurrent
    );
    return result;
  });
}

export async function acceptCatalog(
  write: ProviderWrite,
  intent: MutationIntent,
  digest: string,
  models: string[]
) {
  const current = write.writable();
  if (!current || !models.length) return;
  return write.run('catalog-accept', async (isCurrent) => {
    const submission = intent.prepare(
      { providerId: current.id, digest, models },
      () => [{ id: current.id, etag: current.etag }, digest, models] as const
    );
    await acceptCatalogSuggestions(...submission.input, submission.requestId);
    if (!isCurrent()) return;
    intent.reset();
    const count = submission.input[2].length;
    await write.observe(
      activationReceipt(
        current.id,
        `Catalog facts stored for ${count} model${count === 1 ? '' : 's'}. Certify the provider's capabilities again before activation.`
      ),
      isCurrent
    );
    return true;
  });
}
