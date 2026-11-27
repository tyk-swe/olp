import assert from 'node:assert/strict';
import test from 'node:test';
import { operationsFrom } from './generate-management-operations.mjs';

const baseDocument = {
  components: {
    schemas: {
      Widget: {
        type: 'object',
        properties: {
          name: { type: 'string' },
          nested: { $ref: '#/components/schemas/Nested' }
        }
      },
      Nested: { type: 'object', properties: { count: { type: 'integer' } } }
    },
    parameters: {
      IfMatch: {
        name: 'If-Match',
        in: 'header',
        required: true,
        schema: { type: 'string' }
      }
    }
  },
  paths: {
    '/api/v1/mcp': {
      post: {
        operationId: 'mcp_endpoint',
        security: [{ managementToken: ['widgets.read'] }],
        responses: { 200: { content: { 'application/json': {} } } }
      }
    },
    '/api/v1/session-only': {
      get: {
        operationId: 'session_only',
        security: [{ sessionCookie: ['widgets.read'] }],
        responses: { 200: { content: { 'application/json': {} } } }
      }
    },
    '/api/v1/widgets/{id}': {
      parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
      patch: {
        operationId: 'update_widget',
        tags: ['widgets'],
        summary: 'Update widget',
        security: [{ managementToken: ['widgets.write'] }],
        parameters: [
          { $ref: '#/components/parameters/IfMatch' },
          { name: 'dry_run', in: 'query', schema: { type: 'boolean' } }
        ],
        requestBody: {
          required: true,
          content: { 'application/json': { schema: { $ref: '#/components/schemas/Widget' } } }
        },
        responses: {
          200: {
            headers: {
              ETag: {},
              'OLP-Parent-ETag': {},
              'X-Internal-Secret': {}
            },
            content: { 'application/json': {} }
          }
        }
      }
    },
    '/api/v1/download': {
      get: {
        operationId: 'download_blob',
        security: [{ managementToken: ['widgets.read'] }],
        responses: { 200: { content: { 'application/octet-stream': {} } } }
      }
    }
  }
};

test('operationsFrom bundles schema refs, filters non-JSON and non-token routes, and allowlists headers', () => {
  const ops = operationsFrom(baseDocument);
  assert.equal(ops.length, 1);
  const [op] = ops;
  assert.equal(op.name, 'update_widget');
  assert.equal(op.read_only, false);
  assert.deepEqual(op.response_headers, { ETag: 'etag', 'OLP-Parent-ETag': 'parent_etag' });
  assert.deepEqual(Object.keys(op.input_schema.$defs).sort(), ['Nested', 'Widget']);
  assert.deepEqual(op.input_schema.required, ['path', 'if_match', 'body']);
});

test('operationsFrom rejects unsupported parameters and invalid x-olp-read-only annotations', () => {
  const badReadOnly = {
    components: { schemas: {} },
    paths: {
      '/api/v1/bad': {
        post: {
          operationId: 'bad_ro',
          'x-olp-read-only': 'yes',
          security: [{ managementToken: ['widgets.read'] }],
          responses: { 200: {} }
        }
      }
    }
  };
  assert.throws(() => operationsFrom(badReadOnly), /Invalid reviewed read-only effect/);

  const badHeader = {
    components: { schemas: {} },
    paths: {
      '/api/v1/bad-header': {
        get: {
          operationId: 'bad_header',
          security: [{ managementToken: ['widgets.read'] }],
          parameters: [{ name: 'X-Custom', in: 'header', schema: { type: 'string' } }],
          responses: { 200: {} }
        }
      }
    }
  };
  assert.throws(() => operationsFrom(badHeader), /Unsupported management parameter header X-Custom/);
});
