import { parseJsonObject } from '#lib/editing/json.js';
import { validated } from '#lib/editing/validation.js';
import type { RoutingPolicy, RouteTemplate, TemplateInput } from './api';
import type { FidelityMode } from './routeFidelity';

export type TemplateDraft = {
  name: string;
  project: string;
  selector: string;
  filter: string;
  slug: string;
  timeout: number;
  attempts: number;
  fidelity: FidelityMode;
  policy: string;
  publish: boolean;
};

export type TemplateField = Exclude<keyof TemplateDraft, 'publish'>;

export const templateServerFields: Readonly<Record<string, TemplateField>> = {
  name: 'name',
  project_id: 'project',
  provider_selector: 'selector',
  model_filter: 'filter',
  slug_pattern: 'slug',
  overall_timeout_ms: 'timeout',
  max_attempts: 'attempts',
  fidelity: 'fidelity',
  allowed_strategies: 'policy',
  constraints: 'policy',
  defaults: 'policy'
};

export function templateDraft(saved: RouteTemplate | null): TemplateDraft {
  return {
    name: saved?.name ?? '',
    project: saved?.project_id ?? '',
    selector: saved?.provider_selector ?? '',
    filter: saved?.model_filter ?? '*',
    slug: saved?.slug_pattern ?? '{model}',
    timeout: saved?.overall_timeout_ms ?? 60000,
    attempts: saved?.max_attempts ?? 2,
    fidelity: saved?.fidelity.mode ?? 'strict',
    policy: saved?.routing_policy
      ? JSON.stringify(saved.routing_policy, null, 2)
      : '',
    publish: saved?.auto_publish ?? false
  };
}

export function validateTemplate(draft: TemplateDraft) {
  const policy = draft.policy.trim() ? parseJsonObject(draft.policy) : null;
  return validated<TemplateInput, TemplateField>(
    draft.policy.trim() && !policy
      ? { policy: 'Routing policy must be a JSON object' }
      : {},
    () => ({
      name: draft.name,
      project_id: draft.project || null,
      provider_selector: draft.selector,
      model_filter: draft.filter,
      slug_pattern: draft.slug,
      overall_timeout_ms: draft.timeout,
      max_attempts: draft.attempts,
      fidelity: { mode: draft.fidelity },
      routing_policy: policy as RoutingPolicy | null,
      auto_publish: draft.publish
    })
  );
}
