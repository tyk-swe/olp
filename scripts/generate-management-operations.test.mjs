import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
import test from 'node:test';
import { operationsFrom } from './generate-management-operations.mjs';

test('generated management commands match the checked-in contract', () => {
  const document = JSON.parse(readFileSync('openapi/management.json', 'utf8'));
  const generated = JSON.parse(readFileSync('sdk/management/operations.gen.json', 'utf8'));
  assert.deepEqual(generated, operationsFrom(document));
  assert(generated.length > 180);
  assert(!generated.some((op) => op.name === 'management_mcp' || op.name === 'setup'));
});

test('tool schemas retain recursive references and required conditions', () => {
  const document = {
    components: { schemas: { Node: { type: 'object', properties: { next: { $ref: '#/components/schemas/Node' } } } } },
    paths: { '/api/v1/nodes/{id}': { put: {
      operationId: 'put_node', security: [{ managementToken: ['configure'] }],
      parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }, { name: 'If-Match', in: 'header', required: true, schema: { type: 'string' } }],
      requestBody: { required: true, content: { 'application/json': { schema: { $ref: '#/components/schemas/Node' } } } }
    } } }
  };
  const [operation] = operationsFrom(document);
  assert.deepEqual(operation.input_schema.required, ['path', 'if_match', 'body']);
  assert.equal(operation.input_schema.$defs.Node.properties.next.$ref, '#/$defs/Node');
});

test('JSON clients omit downloads, SSE and XML but retain JSON and empty successes', () => {
  const paths = Object.fromEntries(['application/octet-stream', 'text/event-stream', 'application/samlmetadata+xml', 'application/scim+json', 'application/json', null].map((type, index) => [`/api/v1/test-${index}`, { get: { operationId: `test_${index}`, security: [{ managementToken: ['read'] }], responses: { 200: type ? { content: { [type]: { schema: { type: 'object' } } } } : {} } } }]));
  assert.deepEqual(operationsFrom({ paths }).map((operation) => operation.name), ['test_3', 'test_4', 'test_5']);
});


test('parameter serialization facts admit current scalar shapes and reject unsupported contracts', () => {
  const operation = (parameter) => operationsFrom({ paths: { '/api/v1/test': { get: {
    operationId: 'test', security: [{ managementToken: ['read'] }], parameters: [parameter]
  } } } });
  for (const type of ['string', 'integer', 'boolean']) {
    const [result] = operation({ name: 'value', in: 'query', schema: { type } });
    assert.equal(result.parameters[0].type, type);
  }
  for (const parameter of [
    { name: 'value', in: 'query', schema: { type: 'array', items: { type: 'string' } } },
    { name: 'value', in: 'query', schema: { type: 'object' } },
    { name: 'value', in: 'query', schema: { type: 'integer' }, style: 'deepObject' },
    { name: 'value', in: 'cookie', schema: { type: 'string' } },
    { name: 'X-Custom', in: 'header', schema: { type: 'string' } },
    { name: 'value', in: 'path', schema: { type: 'integer' }, required: true }
  ]) assert.throws(() => operation(parameter), /Unsupported management parameter/);
});

test('response facts and reviewed read-only POST effects come from the contract', () => {
  const document = JSON.parse(readFileSync('openapi/management.json', 'utf8'));
  const operations = operationsFrom(document);
  for (const name of ['scim_users_search', 'scim_groups_search', 'plan_configuration', 'lookup_end_user']) {
    assert.equal(operations.find((op) => op.name === name).read_only, true);
  }
  assert.equal(operations.find((op) => op.name === 'validate_route_draft').read_only, false);
  assert.deepEqual(operations.find((op) => op.name === 'delete_credential_slot').response_headers, {
    ETag: 'etag', 'OLP-Previous-Parent-ETag': 'previous_parent_etag', 'OLP-Parent-ETag': 'parent_etag'
  });
});
