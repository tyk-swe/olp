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
      parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }, { name: 'If-Match', in: 'header', required: true }],
      requestBody: { required: true, content: { 'application/json': { schema: { $ref: '#/components/schemas/Node' } } } }
    } } }
  };
  const [operation] = operationsFrom(document);
  assert.deepEqual(operation.input_schema.required, ['path', 'if_match', 'body']);
  assert.equal(operation.input_schema.$defs.Node.properties.next.$ref, '#/$defs/Node');
});
