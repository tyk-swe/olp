import { apiClient } from '#lib/session/client.js';
import {
  unwrap,
  unwrapPage,
  createHeaders,
  ifMatch
} from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import type { components } from '#lib/management/schema.js';
export type Issuer = components['schemas']['WorkloadIssuer'];
export type IssuerWrite = components['schemas']['WorkloadIssuerWrite'];
export async function listIssuers(signal?: AbortSignal) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/workload-issuers', {
        params: { query: { cursor } },
        signal
      })
    )
  );
}
export async function saveIssuer(
  body: IssuerWrite,
  requestId: string,
  current?: Issuer
) {
  if (current)
    return unwrap(
      await apiClient.PUT('/api/v1/workload-issuers/{issuer_id}', {
        params: {
          path: { issuer_id: current.id },
          header: ifMatch(current.etag)
        },
        body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/workload-issuers', {
      params: { header: createHeaders(requestId) },
      body
    })
  );
}
export function issuerInput(issuer: Issuer): IssuerWrite {
  const {
    name,
    issuer: url,
    jwks_url,
    enabled,
    audiences,
    algorithms,
    max_lifetime_seconds,
    disabled_key_ids,
    mappings
  } = issuer;
  return {
    name,
    issuer: url,
    jwks_url,
    enabled,
    audiences,
    algorithms,
    max_lifetime_seconds,
    disabled_key_ids,
    mappings
  };
}

export const defaultIssuerMappings = [
  {
    name: 'build',
    match: { '/repository': 'example/repository' },
    project_id: '',
    limit_template: 'worker',
    route_groups: ['generation'],
    scopes: ['inference'],
    end_user_claim: null,
    allow_provider_state: false
  }
];

const words = (text: string) => text.split(/[\s,]+/).filter(Boolean);

export function buildIssuerWrite(fields: {
  name: string;
  issuer: string;
  jwks: string;
  enabled: boolean;
  audiences: string;
  algorithms: string[];
  lifetime: string;
  disabledKeys: string;
  mappings: string;
}): IssuerWrite {
  const parsed: unknown = JSON.parse(fields.mappings);
  if (!Array.isArray(parsed) || parsed.length < 1 || parsed.length > 32)
    throw new Error('Provide one to 32 claim mappings.');
  const seconds = Number(fields.lifetime);
  if (
    !Number.isInteger(seconds) ||
    seconds < 60 ||
    seconds > 86400 ||
    fields.algorithms.length === 0
  )
    throw new Error(
      'Choose a signature algorithm and a maximum lifetime between 60 and 86400 seconds.'
    );
  return {
    name: fields.name.trim(),
    issuer: fields.issuer,
    jwks_url: fields.jwks,
    enabled: fields.enabled,
    audiences: words(fields.audiences),
    algorithms: fields.algorithms as IssuerWrite['algorithms'],
    max_lifetime_seconds: seconds,
    disabled_key_ids: words(fields.disabledKeys),
    mappings: parsed as IssuerWrite['mappings']
  };
}
