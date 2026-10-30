import AxeBuilder from '@axe-core/playwright';
import { execFileSync } from 'node:child_process';
import { randomBytes, randomUUID } from 'node:crypto';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';
import type {
  CodeAccount,
  CodePool,
  CodeRoute
} from '../../src/lib/api/code-mode';

test('manages real project pools, draft routes and publications without inference', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const suffix = randomUUID().slice(0, 8);
  const project = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/projects',
    { body: { name: `Code mode ${suffix}` }, idempotency: randomUUID() }
  );
  expect(project.status).toBe(201);
  await page.goto('/code-mode');
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(project.body.id);
  await expect(page.getByText('No accounts in this project.')).toBeVisible();
  await page.getByRole('button', { name: 'Pools', exact: true }).click();
  await page.getByRole('button', { name: 'Create pool', exact: true }).click();
  await page.getByLabel('Name', { exact: true }).fill(`Shared ${suffix}`);
  await page.getByLabel('Pool kind').selectOption('shared');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('None — no key can use this pool')).toBeVisible();
  const pools = await manage<{ items: CodePool[] }>(
    page,
    'GET',
    `/api/v1/code/pools?project_id=${project.body.id}`
  );
  const pool = pools.body.items.find(
    (item) => item.name === `Shared ${suffix}`
  )!;
  expect(pool.account_ids).toEqual([]);
  expect(pool.api_key_ids).toEqual([]);
  // Seed the same current-grant metadata as the Go code-mode fixtures. The
  // browser exercises real account, pool and publication APIs without login
  // to a subscription service or an inference probe.
  // A time-ordered identifier, as the server assigns, so the fixture lists
  // among providers by creation like any other.
  const providerID = uuidv7();
  const credentialID = randomUUID();
  const profile = await manage<{ id: string }>(page, 'GET', '/api/v1/profile');
  const databaseURL = new URL(process.env.OLP_DATABASE_URL!);
  execFileSync(
    'psql',
    [
      '-X',
      '--set=ON_ERROR_STOP=1',
      `--set=provider=${providerID}`,
      `--set=credential=${credentialID}`,
      `--set=project=${project.body.id}`,
      `--set=owner=${profile.body.id}`,
      `--set=etag=${randomUUID()}`,
      `--set=slots_etag=${randomUUID()}`,
      '--file=-'
    ],
    {
      env: {
        ...process.env,
        PGHOST: databaseURL.searchParams.get('host') ?? databaseURL.hostname,
        PGPORT: databaseURL.port || '5432',
        PGUSER: decodeURIComponent(databaseURL.username),
        PGPASSWORD: decodeURIComponent(databaseURL.password),
        PGDATABASE: `${process.env.OLP_CONSOLE_E2E_DATABASE_PREFIX ?? ''}olp_packaged`,
        PGSSLMODE: databaseURL.searchParams.get('sslmode') ?? 'prefer',
        PGSSLROOTCERT: databaseURL.searchParams.get('sslrootcert') ?? ''
      },
      input: `
INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id)
VALUES(:'provider','Fixture coding subscription','plugin','draft',
  '{"kind":"plugin","auth_mode":"grant","endpoint":"https://fixture.invalid","profile_id":"codex-subscription","profile_revision":"fixture-digest"}',
  :'etag',:'slots_etag',:'owner',:'project');
INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,profile_id,principal,grant_facts)
VALUES(:'credential',:'provider',1,'fixture-digest','codex-subscription','fixture-principal','{}');
INSERT INTO olp.provider_grants(credential_id) VALUES(:'credential');
`,
      stdio: ['pipe', 'pipe', 'pipe']
    }
  );
  const account = await manage<CodeAccount>(
    page,
    'POST',
    '/api/v1/code/accounts',
    {
      body: {
        project_id: project.body.id,
        provider_id: providerID,
        credential_id: credentialID,
        name: `Subscription ${suffix}`,
        enabled: true,
        models: [
          'fixture-native-model',
          'fixture-other-model',
          'unsaved-native-model'
        ]
      },
      idempotency: randomUUID()
    }
  );
  expect(account.status).toBe(201);
  const assignedPool = await manage<CodePool>(
    page,
    'PUT',
    `/api/v1/code/pools/${pool.id}`,
    {
      body: {
        project_id: project.body.id,
        name: pool.name,
        kind: pool.kind,
        owner_user_id: pool.owner_user_id,
        account_ids: [account.body.id],
        api_key_ids: []
      },
      etag: pool.etag
    }
  );
  expect(assignedPool.status).toBe(200);
  await page.getByRole('button', { name: 'Routes', exact: true }).click();
  await page.getByRole('button', { name: 'Create route', exact: true }).click();
  await page.getByLabel('Route slug').fill(`code-${suffix}`);
  await page.getByLabel('Pool', { exact: true }).selectOption(pool.id);
  await page
    .getByLabel('Native models')
    .fill('fixture-native-model\nfixture-other-model');
  await page
    .getByLabel('Rewrite model identifiers using exact aliases')
    .check();
  await page
    .getByLabel('Exact model aliases')
    .fill('team-code=fixture-native-model');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Not published', { exact: true })).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  await page
    .getByRole('button', { name: 'Publish route', exact: true })
    .click();
  await expect(
    page.getByText('Route published without synthetic inference.', {
      exact: false
    })
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Revisions and client setup' })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Published route revisions' })
  ).toBeVisible();
  await expect(page.getByText('Revision 1', { exact: true })).toBeVisible();
  const routes = await manage<{ items: CodeRoute[] }>(
    page,
    'GET',
    `/api/v1/code/routes?project_id=${project.body.id}`
  );
  const route = routes.body.items[0];
  expect(route.models).toEqual(['fixture-native-model', 'fixture-other-model']);
  expect(route.enabled).toBe(true);
  expect(route.model_rewriting).toBe(true);
  expect(route.aliases).toEqual({ 'team-code': 'fixture-native-model' });
  expect(route.revision).toBe(1);
  await expect(page.getByLabel('Client model')).toHaveValue(
    'fixture-native-model'
  );
  const model = 'fixture-other-model';
  await page.getByLabel('Client model').focus();
  await Promise.all([
    page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname.endsWith('/client-config') &&
        url.searchParams.get('model') === model
      );
    }),
    page.getByLabel('Client model').selectOption(model)
  ]);
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    new RegExp(`model = "${model}"`)
  );
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    new RegExp(`"X-OLP-Code-Model" = "${model}"`)
  );
  await expect(page.getByLabel('Client model')).toBeFocused();
  const gatewayURL = 'https://gateway.example';
  await Promise.all([
    page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname.endsWith('/client-config') &&
        url.searchParams.get('gateway_url') === gatewayURL &&
        url.searchParams.get('model') === model
      );
    }),
    page.getByLabel('Public gateway URL').fill(gatewayURL)
  ]);
  await expect(page.getByLabel('Project', { exact: true })).toHaveValue(
    project.body.id
  );
  await expect(
    page.getByRole('button', { name: 'Routes', exact: true })
  ).toHaveAttribute('aria-pressed', 'true');
  await expect(
    page.getByRole('heading', { name: 'Published route revisions' })
  ).toBeVisible();
  await expect(page.getByLabel('Client model')).toHaveValue(model);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: info.outputPath('code-client-model-mobile.png'),
    fullPage: true
  });
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.getByRole('button', { name: 'Edit draft' }).click();
  await expect(page.getByLabel('Maximum request body (bytes)')).toBeVisible();
  await page.getByLabel('Native models').fill('unsaved-native-model');
  await page
    .getByLabel('Public gateway URL')
    .fill('https://changed-gateway.example');
  await expect(page.getByLabel('Native models')).toHaveValue(
    'unsaved-native-model'
  );
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByRole('alert')).toContainText(
    'targeting a listed native model'
  );
  await page
    .getByLabel('Exact model aliases')
    .fill('team-code=unsaved-native-model');
  await expect(page.getByLabel('Client model')).toHaveValue(model);
  const [savedDraft] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'PUT' &&
        new URL(response.url()).pathname.endsWith(`/code/routes/${route.id}`)
    ),
    page.getByRole('button', { name: 'Save', exact: true }).click()
  ]);
  expect(savedDraft.status()).toBe(200);
  expect((await savedDraft.json()).aliases).toEqual({
    'team-code': 'unsaved-native-model'
  });
  await expect(page.getByLabel('Client model')).toHaveValue(model);
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    new RegExp(`"X-OLP-Code-Model" = "${model}"`)
  );
  page.once('dialog', (dialog) => dialog.accept());
  const [publication, configuration] = await Promise.all([
    page.waitForResponse((response) =>
      new URL(response.url()).pathname.endsWith('/publish')
    ),
    page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname.endsWith('/client-config') &&
        !url.searchParams.has('model')
      );
    }),
    page.getByRole('button', { name: 'Publish route', exact: true }).click()
  ]);
  expect(publication.status()).toBe(200);
  expect(configuration.status()).toBe(200);
  await expect(page.getByLabel('Client model')).toHaveValue(
    'unsaved-native-model'
  );
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    /"X-OLP-Code-Model" = "unsaved-native-model"/
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

function uuidv7(): string {
  const bytes = randomBytes(16);
  bytes.writeUIntBE(Date.now(), 0, 6);
  bytes[6] = (bytes[6] & 0x0f) | 0x70;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = bytes.toString('hex');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

test('requires a new configuration plan after changing code destination bindings', async ({
  page
}) => {
  await signInGatewayOwner(page);
  await page.goto('/settings');
  const promotion = page.getByRole('region', {
    name: 'Configuration promotion'
  });
  await promotion
    .getByLabel('Artifact', { exact: true })
    .fill(
      JSON.stringify({ api_version: 'openllmproxy.dev/config/v2', code: {} })
    );
  await promotion.getByRole('button', { name: 'Plan', exact: true }).click();
  await expect(
    promotion.getByRole('button', { name: 'Apply', exact: true })
  ).toBeVisible();
  await promotion.getByLabel('Code destination bindings').fill('{}');
  await expect(
    promotion.getByRole('button', { name: 'Apply', exact: true })
  ).toHaveCount(0);
  await promotion.getByRole('button', { name: 'Plan', exact: true }).click();
  await expect(
    promotion.getByRole('button', { name: 'Apply', exact: true })
  ).toBeVisible();
});
