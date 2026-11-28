import type {
  Draft,
  Kind,
  MCPServer,
  MCPServerUpdate,
  Publication,
  ToolPin
} from './api';
import { validated, type Validation } from '#lib/editing/validation.js';

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
export type ServerPricingField =
  Exclude<keyof ServerPricingForm, 'toolPrices'> | `toolPrice:${string}`;
type CatalogTools = readonly { name: string }[];
type ServerPricing = Pick<
  MCPServerUpdate,
  | 'unit_price'
  | 'tool_prices'
  | 'calls_per_minute'
  | 'max_concurrency'
  | 'per_key_calls_per_minute'
  | 'per_key_max_concurrency'
>;
function unitPriceError(price: string) {
  return !price || pricePattern.test(price)
    ? undefined
    : 'Use a non-negative decimal unit price, 0 for free, or blank for unpriced.';
}
function toolPriceError(name: string, price: string, tools: CatalogTools) {
  if (!price) return undefined;
  if (!tools.some((tool) => tool.name === name))
    return `Price only certified tools. ${name} is not in the catalog.`;
  return pricePattern.test(price)
    ? undefined
    : `Use a non-negative decimal price for ${name}.`;
}
function limitError(raw: string, ceiling: number, label: string) {
  const value = Number(raw);
  return /^[0-9]+$/.test(raw.trim()) && value >= 1 && value <= ceiling
    ? undefined
    : `${label} must be a whole number from 1 to ${ceiling}.`;
}
export function validateServerPricing(
  tools: CatalogTools,
  form: ServerPricingForm
): Validation<ServerPricing, ServerPricingField> {
  const unitPrice = form.unitPrice.trim();
  const toolPrices = Object.entries(form.toolPrices).map(
    ([name, raw]) => [name, raw.trim()] as const
  );
  return validated<ServerPricing, ServerPricingField>(
    {
      unitPrice: unitPriceError(unitPrice),
      ...Object.fromEntries(
        toolPrices.map(([name, price]) => [
          `toolPrice:${name}`,
          toolPriceError(name, price, tools)
        ])
      ),
      callsPerMinute: limitError(
        form.callsPerMinute,
        60000,
        'Calls per minute'
      ),
      maxConcurrency: limitError(form.maxConcurrency, 1024, 'Max concurrency'),
      perKeyCallsPerMinute: limitError(
        form.perKeyCallsPerMinute,
        60000,
        'Per-key calls per minute'
      ),
      perKeyMaxConcurrency: limitError(
        form.perKeyMaxConcurrency,
        1024,
        'Per-key max concurrency'
      )
    },
    () => ({
      unit_price: unitPrice || null,
      tool_prices: Object.fromEntries(toolPrices.filter(([, price]) => price)),
      calls_per_minute: Number(form.callsPerMinute),
      max_concurrency: Number(form.maxConcurrency),
      per_key_calls_per_minute: Number(form.perKeyCallsPerMinute),
      per_key_max_concurrency: Number(form.perKeyMaxConcurrency)
    })
  );
}
export type ServerPricingResult =
  { ok: true; input: MCPServerUpdate } | { ok: false; message: string };
export function serverPricingInput(
  server: Pick<MCPServer, 'name' | 'enabled' | 'endpoint'> & {
    catalog: { tools: CatalogTools };
  },
  form: ServerPricingForm
): ServerPricingResult {
  const prices = validateServerPricing(server.catalog.tools, form);
  if (!prices.ok) {
    const [message = ''] = Object.values(prices.fieldErrors).filter(
      (error) => error !== undefined
    );
    return { ok: false, message };
  }
  const { name, endpoint, enabled } = server;
  return { ok: true, input: { name, endpoint, enabled, ...prices.value } };
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
  const base = { project_id: form.project, name: form.name.trim() };
  if (kind === 'toolsets')
    return {
      ...base,
      toolset: { tools: form.tools.map((tool) => ({ ...tool })) }
    };
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
    const prompt: NonNullable<Draft['prompt']> =
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
    return { ...base, prompt };
  }
  return {
    ...base,
    agent: {
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
    }
  };
}
export type PublicationIdentity = Pick<Publication, 'id' | 'etag'>;
export type PublicationAction =
  'save' | 'publish' | 'retire' | 'production' | 'staging';
export type PublicationCommand =
  | { action: 'create'; kind: Kind; input: Draft }
  | {
      action: 'update';
      kind: Kind;
      observed: PublicationIdentity;
      input: Draft;
    }
  | { action: 'publish' | 'retire'; kind: Kind; observed: PublicationIdentity }
  | {
      action: 'label';
      observed: PublicationIdentity;
      label: 'production' | 'staging';
      revision: string;
    };
export type PublicationCommandResult =
  { ok: true; command: PublicationCommand } | { ok: false; message: string };

type PublicationEdits = {
  kind: Kind;
  current: Publication | null;
  action: PublicationAction;
  form: PublicationForm;
  credential: string;
  entraClientSecret: string;
  removeCredential: boolean;
  revision: string;
};

export function publicationCommand(
  edits: PublicationEdits
): PublicationCommandResult {
  const { kind, current, action, form } = edits;
  if (action === 'save') {
    const draft = publicationInput(kind, form);
    const input: Draft =
      kind !== 'agents'
        ? draft
        : {
            ...draft,
            ...(form.oauthEnabled || edits.removeCredential
              ? { credential: null }
              : edits.credential
                ? { credential: edits.credential }
                : {}),
            ...(form.oauthEnabled && form.oauthEntra && edits.entraClientSecret
              ? { entra_client_secret: edits.entraClientSecret }
              : {})
          };
    return {
      ok: true,
      command: current
        ? {
            action: 'update',
            kind,
            observed: { id: current.id, etag: current.etag },
            input
          }
        : { action: 'create', kind, input }
    };
  }
  if (!current) return { ok: false, message: 'Save the draft first.' };
  const observed = { id: current.id, etag: current.etag };
  if (action === 'publish' || action === 'retire')
    return { ok: true, command: { action, kind, observed } };
  if (kind !== 'prompts')
    return { ok: false, message: 'Only prompts have publication labels.' };
  if (!edits.revision)
    return { ok: false, message: 'Choose a revision first.' };
  return {
    ok: true,
    command: {
      action: 'label',
      observed,
      label: action,
      revision: edits.revision
    }
  };
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
