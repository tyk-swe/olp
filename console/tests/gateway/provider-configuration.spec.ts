import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '../playwright';

const endpoint = 'http://127.0.0.1:4187/v1';
const model = 'compatible-e2e-model';
// These are native number tokens, not JavaScript numbers. Integer-like schema
// member order and __proto__ are intentional parts of the independent oracle.
const corpus =
  '{"negative_zero":-0,"tiny_exponent":1e-1000,"long_decimal":0.1000000000000000000001,"unsafe_integer":9007199254740993,"null":null,"false":false,"zero":0,"empty":"","empty_array":[],"ordered":[false,0,"",null],"schema":{"properties":{"10":{"type":"string"},"2":{"type":"number"},"__proto__":{"inert":true}},"required":["10","2"]},"unknown":{"blocks":[{"id":"second"},{"id":"first"}]}}';
const configuration = `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"${endpoint}","profile_id":"compatible-chat","profile_revision":"1","options":{"operation_defaults":{"generation":{"dialect":"openai-chat","values":{"seed":9007199254740993},"native_options":{"fixture_provider":${corpus}}}},"bindings":{"${model}":{"defaults":{"generation":{"dialect":"openai-chat","native_options":{"fixture_binding":${corpus}}}}}}}}`;

async function signIn(page: Page) {
  await page.goto('/');
  await expect(page).toHaveURL(/\/(setup|login)(\?.*)?$/);
  if (/\/setup$/.test(page.url())) {
    await page.getByLabel('Display name').fill('Owner');
    await page.getByLabel('Work email').fill('owner@example.com');
    await page
      .getByLabel('Password', { exact: true })
      .fill('a long browser test password');
    await page
      .getByLabel('Confirm password')
      .fill('a long browser test password');
    await page
      .getByLabel('Setup token')
      .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
    await page.getByRole('button', { name: 'Create owner account' }).click();
  } else {
    await page.getByLabel('Email').fill('owner@example.com');
    await page.getByLabel('Password').fill('a long browser test password');
    await page.getByRole('button', { name: 'Sign in' }).click();
  }
  await expect(page).toHaveURL(/\/$/);
}

// The public API receives the original source, never a parsed numeric object.
async function management(
  page: Page,
  method: string,
  path: string,
  source?: string,
  match?: string
) {
  return page.evaluate(
    async ({ method, path, source, match }) => {
      const session = await fetch('/api/v3/sessions/current').then((response) =>
        response.json()
      );
      const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        'X-CSRF-Token': session.csrf_token
      };
      if (match) {
        const current = await fetch(match).then((response) => response.json());
        headers['If-Match'] = `"${current.etag}"`;
      }
      if (method === 'POST') headers['Idempotency-Key'] = crypto.randomUUID();
      const response = await fetch(path, { method, headers, body: source });
      return { status: response.status, source: await response.text() };
    },
    { method, path, source, match }
  );
}
function preservesCorpus(source: string) {
  const compact = source.replace(/\s/g, '');
  expect(compact).toContain(`"fixture_provider":${corpus}`);
  expect(compact).toContain(`"fixture_binding":${corpus}`);
  expect(compact).toContain('"seed":9007199254740993');
}
async function savedProvider(page: Page, path: string) {
  const response = await management(page, 'GET', path);
  expect(response.status).toBe(200);
  preservesCorpus(response.source);
  return response.source;
}
async function save(page: Page, path: string) {
  const completed = page.waitForResponse(
    (response) =>
      response.request().method() === 'PATCH' &&
      new URL(response.url()).pathname === path
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  const response = await completed;
  expect(response.status(), await response.text()).toBe(200);
  preservesCorpus(response.request().postData()!);
  expect(response.request().headers()['if-match']).toMatch(/^"[0-9a-f-]+"$/);
  await expect(
    page.getByText('Provider draft settings saved.', { exact: true })
  ).toBeVisible();
  await savedProvider(page, path);
}

test('configuration forms retain native source through real saves, conflicts and strict activation', async ({
  page,
  request
}, info) => {
  test.setTimeout(240_000);
  await signIn(page);
  expect(
    (await request.post('http://127.0.0.1:4187/__test__/reset')).status()
  ).toBe(204);
  const created = await management(
    page,
    'POST',
    '/api/v3/providers',
    `{"name":"Native editor ${info.project.name}","configuration":${configuration},"model":"${model}","credential":"compatible-provider-secret"}`
  );
  expect(created.status, created.source).toBe(201);
  const id = JSON.parse(created.source).id as string;
  const path = `/api/v3/providers/${id}`;
  await page.goto(`/providers/${id}`);
  await expect(page.getByLabel('API profile')).toHaveValue('compatible-chat@1');
  await page.getByText('Advanced configuration JSON', { exact: true }).click();
  const json = page.getByLabel('Native configuration JSON', { exact: true });
  preservesCorpus(await json.inputValue());

  // Form edits preserve both provider and model-bound native subtrees.
  await page
    .getByLabel('Provider name', { exact: true })
    .fill(`Native editor renamed ${info.project.name}`);
  await page.getByText('Network connection', { exact: true }).click();
  await page.getByLabel('Connect timeout (ms)', { exact: true }).fill('1500');
  await page.getByText('Serving bindings', { exact: true }).click();
  await page
    .getByLabel('Binding Snapshot', { exact: true })
    .fill('reviewed-snapshot');
  preservesCorpus(await json.inputValue());
  await expect(
    page.getByRole('button', { name: 'Test completed draft', exact: true })
  ).toBeDisabled();
  await save(page, path);
  await page.reload();
  await page.getByText('Advanced configuration JSON', { exact: true }).click();
  preservesCorpus(await json.inputValue());
  expect(
    await page.evaluate(() => Object.hasOwn(Object.prototype, 'inert'))
  ).toBe(false);

  // Decoded duplicate names must remain invalid, not silently collapse.
  const original = await json.inputValue();
  await json.fill(
    '{"kind":"openai_compatible","\\u006bind":"openai_compatible","auth_mode":"api_key"}'
  );
  await expect(page.getByText(/Duplicate object member/)).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Save draft', exact: true })
  ).toBeDisabled();
  await json.fill(original);
  await page.getByLabel('API profile').selectOption('');
  preservesCorpus(await json.inputValue());
  await expect(
    page.getByRole('button', { name: 'Remove profile-scoped settings' })
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Save draft', exact: true })
  ).toBeDisabled();
  await page.getByLabel('API profile').selectOption('compatible-chat@1');

  // Null and removal are separate actions against the same advanced source.
  await page.getByText('Operation defaults', { exact: true }).click();
  const native = page.locator('.native-field').filter({
    has: page.getByLabel('generation native options: fixture_provider', {
      exact: true
    })
  });
  await native.getByRole('button', { name: 'Use native null' }).click();
  expect((await json.inputValue()).replace(/\s/g, '')).toContain(
    '"fixture_provider":null'
  );
  await native.getByRole('button', { name: 'Remove value' }).click();
  expect(await json.inputValue()).not.toContain('fixture_provider');
  await json.fill(original);
  await page
    .getByLabel('generation controls: seed', { exact: true })
    .fill('9007199254740993');
  await save(page, path);

  // Metadata reads, previews, edits and saves perform no upstream probes.
  const beforeProbe = await request
    .get('http://127.0.0.1:4187/__test__/requests')
    .then((response) => response.json());
  expect(beforeProbe.requests).toEqual([]);
  await page.locator('.profile-editor').screenshot({
    path: info.outputPath('native-configuration-editor.png'),
    animations: 'disabled'
  });

  // A competing public write must not replace the local document or its ETag.
  await page
    .getByLabel('Provider name', { exact: true })
    .fill('Unsaved local name');
  const local = await json.inputValue();
  const external = await management(
    page,
    'PATCH',
    path,
    `{"name":"Concurrent external name","configuration":${local}}`,
    path
  );
  expect(external.status).toBe(200);
  const conflict = page.waitForResponse(
    (response) =>
      response.request().method() === 'PATCH' &&
      new URL(response.url()).pathname === path
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  expect((await conflict).status()).toBe(412);
  await expect(json).toHaveValue(local);
  await expect(page.getByLabel('Provider name', { exact: true })).toHaveValue(
    'Unsaved local name'
  );
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(page.getByLabel('Provider name', { exact: true })).toHaveValue(
    'Concurrent external name'
  );
  preservesCorpus(await json.inputValue());

  // Explicit local certification and activation exercise the saved draft.
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText(/reviewed tuples passed server certification/)
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Test completed draft', exact: true })
    .click();
  await expect(page.getByText(/Connection succeeded:/)).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate changes', exact: true })
    .click();
  await expect(page.getByText(/Activated in runtime generation/)).toBeVisible();
  const active = JSON.parse(await savedProvider(page, path));
  const revision = await management(
    page,
    'GET',
    `${path}/revisions/${active.active_revision}`
  );
  expect(revision.status).toBe(200);
  preservesCorpus(revision.source);

  // Existing omitted contracts stay omitted on an unrelated save; selecting a
  // strict contract is an explicit migration, saved and activated separately.
  const legacy = await management(
    page,
    'POST',
    '/api/v3/route-drafts',
    `{"slug":"native-editor-${info.project.name}","operations":["generation"],"overall_timeout_ms":10000,"max_attempts":1,"targets":[{"provider_id":"${id}","provider_model":"${model}","priority":0,"weight":1,"timeout_ms":5000}]}`
  );
  expect(legacy.status, legacy.source).toBe(201);
  const routeId = JSON.parse(legacy.source).id as string;
  await page.goto(`/routes/${routeId}`);
  await expect(page.getByLabel('Fidelity mode')).toHaveValue('');
  await page.getByLabel('Maximum attempts').fill('2');
  const savedRoute = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' &&
      new URL(response.url()).pathname === `/api/v3/route-drafts/${routeId}`
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  const saved = await savedRoute;
  expect(saved.status()).toBe(200);
  expect(saved.request().postData()).not.toContain('fidelity');
  await page.getByLabel('Fidelity mode').selectOption('strict');
  await expect(
    page.getByText(/Migration selected: implicit legacy → strict/)
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Activate route', exact: true })
  ).toBeDisabled();
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await expect(
    page.getByText(
      'Draft saved. Validate to preview, or activate directly; activation validates the saved draft.',
      { exact: true }
    )
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
  await page.locator('.fidelity-editor').screenshot({
    path: info.outputPath('strict-route-migration.png'),
    animations: 'disabled'
  });
  await page.goto('/routes/new');
  await expect(page.getByLabel('Fidelity mode')).toHaveValue('strict');
});

test('profile migration, schema fields and write-only network credentials share one draft', async ({
  page,
  request
}, info) => {
  test.setTimeout(120_000);
  await signIn(page);
  expect(
    (await request.post('http://127.0.0.1:4187/__test__/reset')).status()
  ).toBe(204);
  const created = await management(
    page,
    'POST',
    '/api/v3/providers',
    `{"name":"Profile migration ${info.project.name}","configuration":{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"${endpoint}","options":{"parameter_defaults":{"seed":9007199254740993}}},"credential":"compatible-provider-secret","model":"${model}"}`
  );
  expect(created.status, created.source).toBe(201);
  const id = JSON.parse(created.source).id as string;
  const path = `/api/v3/providers/${id}`;
  await page.goto(`/providers/${id}`);
  await page.getByText('Advanced configuration JSON', { exact: true }).click();
  const json = page.getByLabel('Native configuration JSON', { exact: true });
  await page.getByLabel('API profile').selectOption('compatible-chat@1');
  expect((await json.inputValue()).replace(/\s/g, '')).toContain(
    '"parameter_defaults":{"seed":9007199254740993}'
  );
  await expect(
    page.getByRole('button', { name: 'Save draft', exact: true })
  ).toBeDisabled();
  await page
    .getByRole('button', { name: 'Remove legacy parameter defaults' })
    .click();
  await page.getByText('Operation defaults', { exact: true }).click();
  await page
    .getByLabel('Operation for defaults', { exact: true })
    .selectOption('generation');
  await page
    .getByRole('button', { name: 'Add operation defaults', exact: true })
    .click();
  await page
    .getByLabel('generation controls field', { exact: true })
    .selectOption('seed');
  await page
    .getByRole('button', { name: 'Add generation controls field', exact: true })
    .click();
  await expect(
    page.getByRole('button', { name: 'Save draft', exact: true })
  ).toBeDisabled();
  await page
    .getByLabel('generation controls: seed', { exact: true })
    .fill('9007199254740993');
  await page
    .getByLabel('generation native options field', { exact: true })
    .fill('fixture_ordered');
  await page
    .getByRole('button', {
      name: 'Add generation native options field',
      exact: true
    })
    .click();
  await page
    .getByLabel('generation native options: fixture_ordered', { exact: true })
    .fill('[false,0,"",null]');
  await page.getByText('Network connection', { exact: true }).click();
  await page.getByLabel('Max conns per host', { exact: true }).fill('2');
  await page
    .getByText('Add or revoke network credentials', { exact: true })
    .click();
  const secret = page.getByLabel('Network credential JSON (write only)', {
    exact: true
  });
  await secret.fill(
    '{"proxy_username":"browser-fixture","proxy_password":"never-render-this-fixture"}'
  );
  const stored = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === `${path}/network-credentials`
  );
  await page
    .getByRole('button', { name: 'Store and select network credential' })
    .click();
  expect((await stored).status()).toBe(201);
  await expect(secret).toHaveValue('');
  await expect(page.getByLabel('API profile')).toHaveValue('compatible-chat@1');
  await expect(
    page
      .getByLabel('Network credential', { exact: true })
      .locator('option:checked')
  ).toHaveText('Version 1');
  expect(await page.locator('body').innerText()).not.toContain(
    'never-render-this-fixture'
  );
  const metadata = await management(page, 'GET', `${path}/network-credentials`);
  expect(metadata.status).toBe(200);
  expect(metadata.source).not.toContain('never-render-this-fixture');
  expect(metadata.source).not.toContain('proxy_password');
  await page.getByLabel('Network credential', { exact: true }).selectOption('');
  const completed = page.waitForResponse(
    (response) =>
      response.request().method() === 'PATCH' &&
      new URL(response.url()).pathname === path
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  const saved = await completed;
  expect(saved.status(), await saved.text()).toBe(200);
  const wire = saved.request().postData()!;
  expect(wire).toContain('"seed":9007199254740993');
  expect(wire).toContain('"fixture_ordered":[false,0,"",null]');
  expect(wire).toContain('"max_conns_per_host":2');
  expect(wire).not.toContain('credential_id');
  expect(wire).not.toContain('parameter_defaults');
  await expect(
    page.getByText('Provider draft settings saved.', { exact: true })
  ).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  await page
    .getByRole('button', { name: 'Revoke version 1', exact: true })
    .click();
  await expect(page.getByText(/Version 1 · .* · revoked/)).toBeVisible();
  await page
    .locator('.configuration-group')
    .filter({ has: page.getByText('Network connection', { exact: true }) })
    .screenshot({
      path: info.outputPath('profile-network-forms.png'),
      animations: 'disabled'
    });
  const upstream = await request
    .get('http://127.0.0.1:4187/__test__/requests')
    .then((response) => response.json());
  expect(upstream.requests).toEqual([]);
});
