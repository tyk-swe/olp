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
export type ServerPricingResult =
  { ok: true; input: MCPServerUpdate } | { ok: false; message: string };
type LimitResult = { ok: true; value: number } | { ok: false; message: string };
function pricingLimit(
  raw: string,
  ceiling: number,
  label: string
): LimitResult {
  const value = Number(raw);
  if (!/^[0-9]+$/.test(raw.trim()) || value < 1 || value > ceiling)
    return {
      ok: false,
      message: `${label} must be a whole number from 1 to ${ceiling}.`
    };
  return { ok: true, value };
}
export function serverPricingInput(
  server: MCPServer,
  form: ServerPricingForm
): ServerPricingResult {
  const unitPrice = form.unitPrice.trim();
  if (unitPrice !== '' && !pricePattern.test(unitPrice))
    return {
      ok: false,
      message:
        'Use a non-negative decimal unit price, 0 for free, or blank for unpriced.'
    };
  const toolPrices: Record<string, string> = {};
  for (const [name, raw] of Object.entries(form.toolPrices)) {
    const price = raw.trim();
    if (price === '') continue;
    if (!server.catalog.tools.some((tool) => tool.name === name))
      return {
        ok: false,
        message: `Price only certified tools. ${name} is not in the catalog.`
      };
    if (!pricePattern.test(price))
      return {
        ok: false,
        message: `Use a non-negative decimal price for ${name}.`
      };
    toolPrices[name] = price;
  }
  const limits = {
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
  for (const limit of Object.values(limits)) {
    if (!limit.ok) return limit;
  }
  return {
    ok: true,
    input: {
      name: server.name,
      endpoint: server.endpoint,
      enabled: server.enabled,
      unit_price: unitPrice === '' ? null : unitPrice,
      tool_prices: toolPrices,
      calls_per_minute: (limits.calls_per_minute as { value: number }).value,
      max_concurrency: (limits.max_concurrency as { value: number }).value,
      per_key_calls_per_minute: (
        limits.per_key_calls_per_minute as { value: number }
      ).value,
      per_key_max_concurrency: (
        limits.per_key_max_concurrency as { value: number }
      ).value
    }
  };
}

export type PublicationForm = {
  project: string;
  name: string;
  tools: ToolPin[];
  template: string;
  variables: string;
  promptMode: 'template' | 'messages';
  messages: { role: 'system' | 'user' | 'assistant'; content: string }[];
  temperature: string;
  topP: string;
  maxOutputTokens: string;
  protocol: 'a2a-0.3.0' | 'a2a-1.0.0';
  endpoint: string;
  cardDigest: string;
  oauthEnabled: boolean;
  oauthEntra: boolean;
  oauthEntraIssuer: string;
  oauthClientID: string;
  oauthAudience: string;
  oauthScopes: string;
  oauthAuthorizationEndpoint: string;
  oauthTokenEndpoint: string;
  oauthDeviceEndpoint: string;
  pricingRevision: string;
  maxCost: string;
  messagePrice: string;
  remoteWorkBoundSeconds: string;
  maxTaskContinuations: string;
  taskDeadlineSeconds: string;
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
    variables: (
      d?.prompt?.variable_schema?.required ??
      d?.prompt?.variables ??
      []
    ).join(', '),
    promptMode: d?.prompt?.messages?.length ? 'messages' : 'template',
    messages: d?.prompt?.messages?.length
      ? d.prompt.messages.map((message) => ({ ...message }))
      : [{ role: 'user', content: d?.prompt?.template ?? '' }],
    temperature: d?.prompt?.default_parameters?.temperature?.toString() ?? '',
    topP: d?.prompt?.default_parameters?.top_p?.toString() ?? '',
    maxOutputTokens:
      d?.prompt?.default_parameters?.max_output_tokens?.toString() ?? '',
    protocol: d?.agent?.protocol ?? 'a2a-1.0.0',
    endpoint: d?.agent?.endpoint ?? '',
    cardDigest: d?.agent?.card_digest ?? '',
    oauthEnabled: !!d?.agent?.oauth,
    oauthEntra: !!d?.agent?.oauth?.entra_obo,
    oauthEntraIssuer: d?.agent?.oauth?.entra_obo?.issuer ?? '',
    oauthClientID: d?.agent?.oauth?.client_id ?? '',
    oauthAudience: d?.agent?.oauth?.audience ?? '',
    oauthScopes: d?.agent?.oauth?.scopes.join(' ') ?? '',
    oauthAuthorizationEndpoint: d?.agent?.oauth?.authorization_endpoint ?? '',
    oauthTokenEndpoint: d?.agent?.oauth?.token_endpoint ?? '',
    oauthDeviceEndpoint: d?.agent?.oauth?.device_authorization_endpoint ?? '',
    pricingRevision: d?.agent?.pricing_revision_id ?? '',
    maxCost: d?.agent?.max_cost ?? '',
    messagePrice: d?.agent?.message_price ?? '',
    remoteWorkBoundSeconds:
      d?.agent?.remote_work_bound_seconds === undefined
        ? ''
        : String(d.agent.remote_work_bound_seconds),
    maxTaskContinuations:
      d?.agent?.max_task_continuations === undefined
        ? ''
        : String(d.agent.max_task_continuations),
    taskDeadlineSeconds:
      d?.agent?.task_deadline_seconds === undefined
        ? ''
        : String(d.agent.task_deadline_seconds),
    toolsetRevisions: d?.agent?.toolset_revisions.join('\n') ?? '',
    routeRevisions: d?.agent?.route_revisions.join('\n') ?? ''
  };
}
export function publicationInput(kind: Kind, form: PublicationForm): Draft {
  const d: Draft = { project_id: form.project, name: form.name.trim() };
  if (kind === 'toolsets')
    d.toolset = { tools: form.tools.map((tool) => ({ ...tool })) };
  if (kind === 'prompts') {
    const names = resourceIDs(form.variables);
    const defaults = {
      ...(form.temperature.trim() !== ''
        ? { temperature: Number(form.temperature) }
        : {}),
      ...(form.topP.trim() !== '' ? { top_p: Number(form.topP) } : {}),
      ...(form.maxOutputTokens.trim() !== ''
        ? { max_output_tokens: Number(form.maxOutputTokens) }
        : {})
    };
    d.prompt =
      form.promptMode === 'messages'
        ? {
            messages: form.messages.map((message) => ({ ...message })),
            variable_schema: {
              type: 'object',
              properties: Object.fromEntries(
                names.map((name) => [name, { type: 'string' as const }])
              ),
              required: names,
              additionalProperties: false
            },
            ...(Object.keys(defaults).length
              ? { default_parameters: defaults }
              : {})
          }
        : {
            template: form.template,
            variables: names,
            ...(Object.keys(defaults).length
              ? { default_parameters: defaults }
              : {})
          };
  }
  if (kind === 'agents')
    d.agent = {
      protocol: form.protocol,
      endpoint: form.endpoint.trim(),
      card_digest: form.cardDigest.trim(),
      ...(form.oauthEnabled
        ? {
            oauth: {
              client_id: form.oauthClientID.trim(),
              audience: form.oauthAudience.trim(),
              scopes: resourceIDs(form.oauthScopes),
              ...(form.oauthEntra
                ? { entra_obo: { issuer: form.oauthEntraIssuer.trim() } }
                : {
                    authorization_endpoint:
                      form.oauthAuthorizationEndpoint.trim()
                  }),
              token_endpoint: form.oauthTokenEndpoint.trim(),
              ...(!form.oauthEntra && form.oauthDeviceEndpoint.trim()
                ? {
                    device_authorization_endpoint:
                      form.oauthDeviceEndpoint.trim()
                  }
                : {})
            }
          }
        : {}),
      pricing_revision_id: form.pricingRevision.trim(),
      max_cost: form.maxCost.trim(),
      ...(form.messagePrice.trim() !== ''
        ? { message_price: form.messagePrice.trim() }
        : {}),
      ...(form.remoteWorkBoundSeconds.trim() !== ''
        ? {
            remote_work_bound_seconds: Number(
              form.remoteWorkBoundSeconds.trim()
            )
          }
        : {}),
      ...(form.maxTaskContinuations.trim() !== ''
        ? { max_task_continuations: Number(form.maxTaskContinuations.trim()) }
        : {}),
      ...(form.taskDeadlineSeconds.trim() !== ''
        ? { task_deadline_seconds: Number(form.taskDeadlineSeconds.trim()) }
        : {}),
      toolset_revisions: resourceIDs(form.toolsetRevisions),
      route_revisions: resourceIDs(form.routeRevisions)
    };
  return d;
}
export type ToolPinResult =
  { ok: true; pin: ToolPin } | { ok: false; message: string };
export async function toolPin(
  server: MCPServer,
  name: string
): Promise<ToolPinResult> {
  if (
    !server.enabled ||
    server.retired_at ||
    !server.catalog.tools.some((tool) => tool.name === name)
  )
    return {
      ok: false,
      message: 'Choose a tool from an enabled certified server.'
    };
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(name)
  );
  const suffix = [...new Uint8Array(digest).slice(0, 16)]
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('');
  return {
    ok: true,
    pin: {
      server_id: server.id,
      revision_id: server.revision_id,
      catalog_digest: server.catalog.digest,
      name,
      alias: `t_${server.id.replaceAll('-', '')}_${suffix}`
    }
  };
}
