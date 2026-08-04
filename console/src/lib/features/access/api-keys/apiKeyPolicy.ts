import type {
  CreateApiKeyInput,
  UpdateApiKeyInput
} from '$lib/api/management/api-keys';

export type ApiKeyPolicyInput = CreateApiKeyInput & UpdateApiKeyInput;
