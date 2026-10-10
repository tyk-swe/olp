import AxeBuilder from '@axe-core/playwright';
import { execFileSync } from 'node:child_process';
import { randomBytes, randomUUID } from 'node:crypto';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';
import type {
  CodeBudget,
  CodeAccount,
  CodePool,
  CodeRoute
} from '../../src/lib/api/code-mode';

test('manages real project pools, draft routes, publications and overlapping token budgets without inference', async ({
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
  '{"kind":"plugin","auth_mode":"grant","endpoint":"https://fixture.invalid"}',
  :'etag',:'slots_etag',:'owner',:'project');
INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts)
VALUES(:'credential',:'provider',1,'fixture-digest','fixture-principal','{}');
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
  expect(route.revision).toBe(1);
  await expect(page.getByLabel('Client native model')).toHaveValue(
    'fixture-native-model'
  );
  const model = 'fixture-other-model';
  await page.getByLabel('Client native model').focus();
  await Promise.all([
    page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname.endsWith('/client-config') &&
        url.searchParams.get('model') === model
      );
    }),
    page.getByLabel('Client native model').selectOption(model)
  ]);
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    new RegExp(`model = "${model}"`)
  );
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    new RegExp(`"X-OLP-Code-Model" = "${model}"`)
  );
  await expect(page.getByLabel('Client native model')).toBeFocused();
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
  await expect(page.getByLabel('Client native model')).toHaveValue(model);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
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
  await expect(page.getByLabel('Client native model')).toHaveValue(model);
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByLabel('Client native model')).toHaveValue(model);
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
  await expect(page.getByLabel('Client native model')).toHaveValue(
    'unsaved-native-model'
  );
  await expect(page.getByLabel('Generated configuration')).toHaveValue(
    /"X-OLP-Code-Model" = "unsaved-native-model"/
  );
  await page
    .getByRole('button', { name: 'Token budgets', exact: true })
    .click();
  await page
    .getByRole('button', { name: 'Create budget', exact: true })
    .click();
  await page.getByLabel('Daily hard token limit (UTC)').fill('12000');
  await page.getByLabel('Monthly hard token limit (UTC)').fill('200000');
  {
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new URL(response.url()).pathname === '/api/v1/code/budgets'
    );
    await page.getByRole('button', { name: 'Save', exact: true }).click();
    const response = await saved;
    expect(response.status()).toBe(201);
    const body = await response.json();
    expect(body.route_id).toBeNull();
    expect(body.daily_tokens).toBe(12000);
    expect(body.monthly_tokens).toBe(200000);
  }
  await expect(page.getByText('12,000', { exact: true })).toBeVisible();
  await page
    .getByRole('button', { name: 'Create budget', exact: true })
    .click();
  await page.getByLabel('Route scope').selectOption(route.id);
  await page.getByLabel('Daily hard token limit (UTC)').fill('5000');
  {
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new URL(response.url()).pathname === '/api/v1/code/budgets'
    );
    await page.getByRole('button', { name: 'Save', exact: true }).click();
    const response = await saved;
    expect(response.status()).toBe(201);
    const body = await response.json();
    expect(body.route_id).toBe(route.id);
    expect(body.daily_tokens).toBe(5000);
    expect(body.monthly_tokens).toBeNull();
  }
  const budgets = await manage<{ items: CodeBudget[] }>(
    page,
    'GET',
    `/api/v1/code/budgets?project_id=${project.body.id}`
  );
  expect(budgets.body.items).toEqual(
    expect.arrayContaining([
      expect.objectContaining({
        route_id: null,
        daily_tokens: 12000,
        monthly_tokens: 200000
      }),
      expect.objectContaining({
        route_id: route.id,
        daily_tokens: 5000,
        monthly_tokens: null
      })
    ])
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('code-budgets-mobile.png'),
    fullPage: true
  });
});

function uuidv7(): string {
  const bytes = randomBytes(16);
  bytes.writeUIntBE(Date.now(), 0, 6);
  bytes[6] = (bytes[6] & 0x0f) | 0x70;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = bytes.toString('hex');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
