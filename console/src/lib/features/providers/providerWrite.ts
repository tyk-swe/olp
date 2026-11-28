import type { Provider } from '#lib/domain/providers/api.js';
import type { ProviderChangeReceipt } from './providerChanges';

export type ProviderWrite = {
  readonly writable: () => Provider | undefined;
  readonly canWrite: (provider: Provider) => boolean;
  readonly run: <T>(
    label: string,
    action: (isCurrent: () => boolean) => Promise<T>
  ) => Promise<T | undefined>;
  readonly observe: (
    receipt: ProviderChangeReceipt,
    isCurrent: () => boolean
  ) => Promise<void>;
  readonly resetModelPage: () => void;
};

export function activationReceipt(
  providerId: string,
  notice: string,
  snapshot?: ProviderChangeReceipt['snapshot']
): ProviderChangeReceipt {
  return { change: { kind: 'activation', id: providerId }, notice, snapshot };
}
