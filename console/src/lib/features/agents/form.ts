import type { Draft, Kind, MCPServer, Publication, ToolPin } from './api';

export type PublicationForm = {
  project: string;
  name: string;
  tools: ToolPin[];
  template: string;
  variables: string;
  protocol: 'a2a-0.3.0' | 'a2a-1.0.0';
  endpoint: string;
  cardDigest: string;
  pricingRevision: string;
  maxCost: string;
  toolsetRevisions: string;
  routeRevisions: string;
};
export const resourceIDs = (raw: string) => raw.split(/[\s,]+/).filter(Boolean);
export function publicationForm(
  current: Publication | null = null
): PublicationForm {
  const d = current?.draft;
  return {
    project: d?.project_id ?? '',
    name: d?.name ?? '',
    tools: (d?.toolset?.tools ?? []).map((tool) => ({ ...tool })),
    template: d?.prompt?.template ?? '',
    variables: d?.prompt?.variables.join(', ') ?? '',
    protocol: d?.agent?.protocol ?? 'a2a-1.0.0',
    endpoint: d?.agent?.endpoint ?? '',
    cardDigest: d?.agent?.card_digest ?? '',
    pricingRevision: d?.agent?.pricing_revision_id ?? '',
    maxCost: d?.agent?.max_cost ?? '',
    toolsetRevisions: d?.agent?.toolset_revisions.join('\n') ?? '',
    routeRevisions: d?.agent?.route_revisions.join('\n') ?? ''
  };
}
export function publicationInput(kind: Kind, form: PublicationForm): Draft {
  const d: Draft = { project_id: form.project, name: form.name.trim() };
  if (kind === 'toolsets')
    d.toolset = { tools: form.tools.map((tool) => ({ ...tool })) };
  if (kind === 'prompts')
    d.prompt = {
      template: form.template,
      variables: resourceIDs(form.variables)
    };
  if (kind === 'agents')
    d.agent = {
      protocol: form.protocol,
      endpoint: form.endpoint.trim(),
      card_digest: form.cardDigest.trim(),
      pricing_revision_id: form.pricingRevision.trim(),
      max_cost: form.maxCost.trim(),
      toolset_revisions: resourceIDs(form.toolsetRevisions),
      route_revisions: resourceIDs(form.routeRevisions)
    };
  return d;
}
export async function toolPin(
  server: MCPServer,
  name: string
): Promise<ToolPin> {
  if (
    !server.enabled ||
    server.retired_at ||
    !server.catalog.tools.some((tool) => tool.name === name)
  )
    throw new Error('Choose a tool from an enabled certified server.');
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(name)
  );
  const suffix = [...new Uint8Array(digest).slice(0, 16)]
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('');
  return {
    server_id: server.id,
    revision_id: server.revision_id,
    catalog_digest: server.catalog.digest,
    name,
    alias: `t_${server.id.replaceAll('-', '')}_${suffix}`
  };
}
