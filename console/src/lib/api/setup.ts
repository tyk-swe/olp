import type { components } from './schema';
import { apiClient } from './client';
import { ApiProblem, throwApiProblem } from './http';

type Schemas = components['schemas'];

export type SetupStatus = Schemas['SetupStatus'];
export type CreateOwnerInput = Schemas['SetupRequest'];

type SetupUser = Schemas['UserResponse'] & { role: 'owner' };

export type CreateOwnerResponse = Omit<Schemas['SessionResponse'], 'user'> & { user: SetupUser };

export async function getSetupStatus(signal?: AbortSignal): Promise<SetupStatus> {
  const { data, error, response } = await apiClient.GET('/api/v1/setup/status', { signal });
  if (!data) throwApiProblem(error, response);
  const value = data;
  if (typeof value?.setup_required !== 'boolean') {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The setup status response is invalid',
      status: 502
    });
  }
  return value;
}

export async function createOwner(
  input: CreateOwnerInput,
  setupToken: string,
  signal?: AbortSignal
): Promise<CreateOwnerResponse> {
  const { data, error, response } = await apiClient.POST('/api/v1/setup', {
    // The one-time setup token is intentionally sent only as a sensitive
    // request header; it is never included in the JSON owner contract or
    // persisted by the console.
    params: { header: { 'X-OLP-Setup-Token': setupToken } },
    body: input,
    signal
  });
  if (!data) throwApiProblem(error, response);
  const value = data;

  if (
    typeof value?.csrf_token !== 'string' ||
    typeof value.user?.id !== 'string' ||
    typeof value.user?.email !== 'string' ||
    typeof value.user?.display_name !== 'string' ||
    value.user?.role !== 'owner'
  ) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The setup response is invalid',
      status: 502
    });
  }

  return value as CreateOwnerResponse;
}
