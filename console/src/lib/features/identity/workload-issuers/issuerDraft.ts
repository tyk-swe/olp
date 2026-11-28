import { captureResult, type Result } from '#lib/editing/result.js';
import { validated } from '#lib/editing/validation.js';
import { errorMessage } from '#lib/management/http.js';
import { defaultIssuerMappings, type Issuer, type IssuerWrite } from './api';

export type IssuerDraft = {
  name: string;
  issuer: string;
  jwks: string;
  enabled: boolean;
  audiences: string;
  algorithms: IssuerWrite['algorithms'];
  lifetime: string;
  disabledKeys: string;
  mappings: string;
};

export type IssuerField = 'algorithms' | 'lifetime' | 'mappings';

export function issuerDraft(saved?: Issuer): IssuerDraft {
  return {
    name: saved?.name ?? '',
    issuer: saved?.issuer ?? '',
    jwks: saved?.jwks_url ?? '',
    enabled: saved?.enabled ?? true,
    audiences: saved?.audiences.join('\n') ?? '',
    algorithms: [...(saved?.algorithms ?? ['RS256' as const])],
    lifetime: String(saved?.max_lifetime_seconds ?? 3600),
    disabledKeys: saved?.disabled_key_ids.join('\n') ?? '',
    mappings: JSON.stringify(saved?.mappings ?? defaultIssuerMappings, null, 2)
  };
}

const words = (text: string) => text.split(/[\s,]+/).filter(Boolean);

function mappingsProblem(parsed: Result<unknown>) {
  if (!parsed.ok)
    return errorMessage(parsed.error, 'Enter the claim mappings as JSON.');
  const { value } = parsed;
  return !Array.isArray(value) || value.length < 1 || value.length > 32
    ? 'Provide one to 32 claim mappings.'
    : undefined;
}

export function validateIssuer(draft: IssuerDraft) {
  const parsed = captureResult((): unknown => JSON.parse(draft.mappings));
  const seconds = Number(draft.lifetime);
  return validated<IssuerWrite, IssuerField>(
    {
      algorithms:
        draft.algorithms.length === 0
          ? 'Choose a signature algorithm.'
          : undefined,
      lifetime:
        !Number.isInteger(seconds) || seconds < 60 || seconds > 86400
          ? 'Enter a lifetime from 60 to 86400 seconds.'
          : undefined,
      mappings: mappingsProblem(parsed)
    },
    () => ({
      name: draft.name.trim(),
      issuer: draft.issuer,
      jwks_url: draft.jwks,
      enabled: draft.enabled,
      audiences: words(draft.audiences),
      algorithms: [...draft.algorithms],
      max_lifetime_seconds: seconds,
      disabled_key_ids: words(draft.disabledKeys),
      mappings: (parsed.ok ? parsed.value : []) as IssuerWrite['mappings']
    })
  );
}
