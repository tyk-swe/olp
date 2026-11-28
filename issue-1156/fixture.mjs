import { createServer } from 'node:http';

const id = '00000000-0000-4000-8000-000000000001';
const rowId = '00000000-0000-4000-8000-000000000002';
const timestamp = '2026-01-01T00:00:00Z';
const member = {
  id: rowId,
  display_name: 'Dana Developer',
  email: 'dev@example.test',
  role: 'developer',
  access_scope: 'global',
  active: true,
  etag: 'fixture-member',
  created_at: timestamp,
  updated_at: timestamp
};

const rows = {
  '/api/v1/pricing/discounts': {
    id: rowId,
    provider_kind: 'openai',
    vendor_id: null,
    provider_id: id,
    provider_name: 'Fixture provider',
    multiplier: '0.90',
    effective_at: timestamp,
    created_at: timestamp,
    created_by: id
  },
  '/api/v1/rate-cards': {
    id: rowId,
    owner: { kind: 'project', project: 'Fixture project', project_id: id },
    revisions: [],
    created_at: timestamp
  },
  '/api/v1/users': member,
  '/api/v1/provider-resources': {
    id: 'fixture-file',
    kind: 'file',
    route: 'fixture-route',
    provider_name: 'Fixture provider',
    upstream_model: 'fixture-model',
    api_key_name: 'Fixture key',
    api_key_id: id,
    state: 'active',
    updated_at: timestamp
  }
};

const replies = {
  '/api/v1/sessions/current': {
    user: { ...member, id, display_name: 'Fixture Owner', email: 'owner@example.test', role: 'owner' },
    csrf_token: 'fixture-csrf',
    operations: ['read', 'access_read', 'access', 'usage', 'settings']
  },
  '/api/v1/setup/status': { setup_required: false },
  '/api/v1/auth/capabilities': { local_login_enabled: true, oidc_login_enabled: false },
  '/api/v1/branding': { name: 'OpenLLMProxy', logo: '', accent: '', accent_scale: null, etag: 'fixture-branding' },
  '/api/v1/provider-kinds': { items: [{ kind: 'openai', label: 'OpenAI-compatible' }] },
  '/api/v1/provider-vendors': []
};

createServer((request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1:45157');
  const row = rows[url.pathname];
  const empty = url.searchParams.has('cursor') || process.env.OLP_FIXTURE_EMPTY === '1';
  const emptyPage = { items: [], next_cursor: null };
  const body = row && !empty
    ? { items: [row], next_cursor: 'fixture-page-2' }
    : replies[url.pathname] ?? emptyPage;
  response.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(body));
}).listen(45157, '127.0.0.1');
