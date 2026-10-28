import type {
  Draft,
  Kind,
  MCPServer,
  MCPServerUpdate,
  Publication,
  ToolPin
} from './api';

export type ServerPricingForm = {
  unitPrice: string;
  toolPrices: Record<string, string>;
  callsPerMinute: string;
  maxConcurrency: string;
  perKeyCallsPerMinute: string;
  perKeyMaxConcurrency: string;
};
const pricePattern = /^[0-9]{1,12}(\.[0-9]{1,12})?$/;
export function serverPricingForm(server: MCPServer): ServerPricingForm {
  return {
    unitPrice: server.unit_price ?? '',
    toolPrices: { ...server.tool_prices },
    callsPerMinute: String(server.calls_per_minute),
    maxConcurrency: String(server.max_concurrency),
    perKeyCallsPerMinute: String(server.per_key_calls_per_minute),
    perKeyMaxConcurrency: String(server.per_key_max_concurrency)
  };
}
function pricingLimit(raw: string, ceiling: number, label: string): number {
  const value = Number(raw);
  if (!/^[0-9]+$/.test(raw.trim()) || value < 1 || value > ceiling)
    throw new Error(`${label} must be a whole number from 1 to ${ceiling}.`);
  return value;
}
export function serverPricingInput(
  server: MCPServer,
  form: ServerPricingForm
): MCPServerUpdate {
  const unitPrice = form.unitPrice.trim();
  if (unitPrice !== '' && !pricePattern.test(unitPrice))
    throw new Error(
      'Use a non-negative decimal unit price, 0 for free, or blank for unpriced.'
    );
  const toolPrices: Record<string, string> = {};
  for (const [name, raw] of Object.entries(form.toolPrices)) {
    const price = raw.trim();
    if (price === '') continue;
    if (!server.catalog.tools.some((tool) => tool.name === name))
      throw new Error(
        `Price only certified tools. ${name} is not in the catalog.`
      );
    if (!pricePattern.test(price))
      throw new Error(`Use a non-negative decimal price for ${name}.`);
    toolPrices[name] = price;
  }
  return {
    name: server.name,
    endpoint: server.endpoint,
    enabled: server.enabled,
    unit_price: unitPrice === '' ? null : unitPrice,
    tool_prices: toolPrices,
    calls_per_minute: pricingLimit(
      form.callsPerMinute,
      60000,
      'Calls per minute'
    ),
    max_concurrency: pricingLimit(form.maxConcurrency, 1024, 'Max concurrency'),
    per_key_calls_per_minute: pricingLimit(
      form.perKeyCallsPerMinute,
      60000,
      'Per-key calls per minute'
    ),
    per_key_max_concurrency: pricingLimit(
      form.perKeyMaxConcurrency,
      1024,
      'Per-key max concurrency'
    )
  };
}

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
