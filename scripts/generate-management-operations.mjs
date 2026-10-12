import { readFileSync, writeFileSync } from 'node:fs';

// A transport-neutral registry shared by the CLI, management MCP and SDK.
// Only operations explicitly admitting management tokens are included.
export function operationsFrom(document) {
  const resolve = (value) => {
    if (!value?.$ref) return value;
    const prefix = '#/components/';
    if (!value.$ref.startsWith(prefix)) throw new Error('External contract reference');
    const [kind, name] = value.$ref.slice(prefix.length).split('/');
    const resolved = document.components[kind]?.[name];
    if (!resolved) throw new Error(`Missing reference ${value.$ref}`);
    return resolved;
  };
  // Standard ETag metadata is always safe when supplied. The contract selects
  // additional allowed mutation facts; no other response headers are exposed.
  const safeResponseHeaders = new Map([
    ['etag', 'etag'],
    ['olp-previous-parent-etag', 'previous_parent_etag'],
    ['olp-parent-etag', 'parent_etag']
  ]);
  const parameterType = (parameter) => {
    const schema = resolve(parameter.schema);
    const type = schema?.type;
    const supported = parameter.in === 'query'
      ? ['string', 'integer', 'boolean'].includes(type) && (parameter.style ?? 'form') === 'form'
      : parameter.in === 'path'
        ? type === 'string' && parameter.required && (parameter.style ?? 'simple') === 'simple'
        : parameter.in === 'header'
          && ['if-match', 'idempotency-key'].includes(parameter.name.toLowerCase())
          && (type === 'string' || (Array.isArray(type) && type.length === 2 && type.includes('string') && type.includes('null')))
          && (parameter.style ?? 'simple') === 'simple';
    if (!supported || parameter.content || parameter.allowReserved || parameter.explode === true && parameter.in !== 'query') {
      throw new Error(`Unsupported management parameter ${parameter.in} ${parameter.name}`);
    }
    return Array.isArray(type) ? 'string' : type;
  };
  const operations = [];
  const names = new Set();
  const bundle = (schema) => {
    const definitions = {};
    const visit = (value) => {
      if (Array.isArray(value)) return value.map(visit);
      if (!value || typeof value !== 'object') return value;
      if (typeof value.$ref === 'string') {
        const prefix = '#/components/schemas/';
        if (!value.$ref.startsWith(prefix)) throw new Error('Unsupported schema reference');
        const name = value.$ref.slice(prefix.length);
        if (!(name in definitions)) {
          definitions[name] = null;
          definitions[name] = visit(resolve(value));
        }
        return { ...value, $ref: `#/$defs/${name}` };
      }
      return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, visit(child)]));
    };
    const result = visit(schema);
    if (Object.keys(definitions).length) result.$defs = definitions;
    return result;
  };
  for (const [path, item] of Object.entries(document.paths)) {
    for (const method of ['get', 'post', 'put', 'patch', 'delete']) {
      const operation = item[method];
      if (!operation) continue;
      const scopes = (operation.security ?? document.security ?? [])
        .filter((alternative) => 'managementToken' in alternative)
        .map((alternative) => alternative.managementToken);
      if (!scopes.length || path === '/api/v1/mcp') continue;
      if ('x-olp-read-only' in operation && typeof operation['x-olp-read-only'] !== 'boolean') {
        throw new Error(`Invalid reviewed read-only effect for ${operation.operationId}`);
      }
      const name = operation.operationId;
      if (!name || names.has(name)) throw new Error(`Duplicate or missing operationId at ${path}`);
      names.add(name);
      const parameters = [...(item.parameters ?? []), ...(operation.parameters ?? [])].map(resolve);
      const properties = {};
      const required = [];
      for (const location of ['path', 'query']) {
        const entries = parameters.filter((p) => p.in === location);
        if (!entries.length) continue;
        const needed = entries.filter((p) => p.required).map((p) => p.name);
        properties[location] = {
          type: 'object',
          properties: Object.fromEntries(entries.map((p) => [p.name, p.schema])),
          additionalProperties: false,
          ...(needed.length ? { required: needed } : {})
        };
        if (needed.length) required.push(location);
      }
      for (const [header, argument] of [['If-Match', 'if_match'], ['Idempotency-Key', 'idempotency_key']]) {
        const parameter = parameters.find((p) => p.in === 'header' && p.name.toLowerCase() === header.toLowerCase());
        if (!parameter) continue;
        properties[argument] = { type: 'string', minLength: 1, description: parameter.description ?? header };
        if (parameter.required) required.push(argument);
      }
      const body = resolve(operation.requestBody);
      if (body) {
        const schema = body.content?.['application/json']?.schema;
        if (!schema) continue; // JSON clients do not expose multipart or raw uploads.
        properties.body = schema;
        if (body.required) required.push('body');
      }
      const successResponses = Object.entries(operation.responses ?? {})
        .filter(([status]) => /^2\d\d$/.test(status))
        .map(([, response]) => resolve(response));
      if (successResponses.some((response) => {
        const types = Object.keys(response.content ?? {});
        return types.length && !types.some((type) => type === 'application/json' || type.endsWith('+json'));
      })) continue; // Download, SSE and XML bodies require their native transports.
      operations.push({
        name,
        group: operation.tags?.[0] ?? 'management',
        method: method.toUpperCase(),
        read_only: operation['x-olp-read-only'] ?? method === 'get',
        path,
        description: operation.summary ?? operation.description ?? name.replaceAll('_', ' '),
        scopes,
        parameters: parameters.map((p) => ({ name: p.name, in: p.in, required: !!p.required, type: parameterType(p) })),
        response_headers: Object.fromEntries([['ETag', 'etag'], ...successResponses.flatMap((response) =>
          Object.keys(response.headers ?? {})
            .filter((name) => safeResponseHeaders.has(name.toLowerCase()))
            .map((name) => [name, safeResponseHeaders.get(name.toLowerCase())])
        )]),
        body_required: !!body?.required,
        input_schema: bundle({ type: 'object', properties, additionalProperties: false, ...(required.length ? { required } : {}) })
      });
    }
  }
  return operations.sort((a, b) => a.name.localeCompare(b.name, 'en'));
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
  const document = JSON.parse(readFileSync('openapi/management.json', 'utf8'));
  writeFileSync('sdk/management/operations.gen.json', JSON.stringify(operationsFrom(document), null, 2) + '\n');
}
