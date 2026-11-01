import { createHash, randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import AxeBuilder from '@axe-core/playwright';
import { expect, type Page, type TestInfo } from '../playwright';

type Identified = { id: string; etag: string };
type Run = { id: string };
type RunDetail = {
  steps: {
    id: string;
    dispatch_id: string;
    state: string;
    observed_cost: string;
    observed_charge: string;
    remaining_cost: string;
    remaining_charge: string;
    tool_alias: string | null;
    tool_status: string | null;
    tool_is_error: boolean | null;
    pricing_state: string | null;
  }[];
  approvals: { id: string; consumed_at: string | null }[];
};

async function trace(runID: string, attemptID: string): Promise<Buffer> {
  const fixture = Buffer.from(
    (
      await readFile(
        new URL('./fixtures/run-trace.pb.base64', import.meta.url),
        'utf8'
      )
    ).trim(),
    'base64'
  );
  return Buffer.from(
    fixture
      .toString('latin1')
      .replace('00000000-0000-0000-0000-000000000001', runID)
      .replace('00000000-0000-0000-0000-000000000002', attemptID),
    'latin1'
  );
}

export async function verifyRunInspection(page: Page, info: TestInfo) {
  const session = await page.request.get('/api/v1/sessions/current');
  expect(session.status()).toBe(200);
  const { csrf_token: csrf } = (await session.json()) as { csrf_token: string };

  async function management<T>(
    path: string,
    method = 'GET',
    data?: unknown,
    etag?: string,
    expected = 200
  ): Promise<T> {
    const response = await page.request.fetch(path, {
      method,
      data,
      headers: {
        Origin: 'http://localhost:4175',
        'X-CSRF-Token': csrf,
        'Idempotency-Key': randomUUID(),
        ...(etag ? { 'If-Match': `"${etag}"` } : {})
      }
    });
    expect(
      response.status(),
      `${method} ${path}: ${await response.text()}`
    ).toBe(expected);
    return (await response.json()) as T;
  }

  const project = await management<Identified>(
    '/api/v1/projects',
    'POST',
    {
      name: 'Run inspection project'
    },
    undefined,
    201
  );
  const server = await management<
    Identified & { revision_id: string; catalog: { digest: string } }
  >(
    '/api/v1/mcp-servers',
    'POST',
    {
      name: 'Run inspection tools',
      project_id: project.id,
      transport: 'streamable_http',
      endpoint: 'http://127.0.0.1:4178/mcp',
      enabled: true,
      unit_price: '0.25'
    },
    undefined,
    201
  );
  const alias = `t_${server.id.replaceAll('-', '')}_${createHash('sha256')
    .update('change')
    .digest('hex')
    .slice(0, 32)}`;
  const toolset = await management<Identified>(
    '/api/v1/toolsets',
    'POST',
    {
      project_id: project.id,
      name: 'Run inspection toolset',
      toolset: {
        tools: [
          {
            server_id: server.id,
            revision_id: server.revision_id,
            catalog_digest: server.catalog.digest,
            name: 'change',
            alias
          }
        ]
      }
    },
    undefined,
    201
  );
  await management(
    `/api/v1/toolsets/${toolset.id}/publish`,
    'POST',
    undefined,
    toolset.etag
  );
  const variants = await Promise.all(
    [{ name: 'Free', unit_price: '0' }, { name: 'Unpriced' }].map(
      async (variant) => {
        const variantServer = await management<
          Identified & { revision_id: string; catalog: { digest: string } }
        >(
          '/api/v1/mcp-servers',
          'POST',
          {
            name: `${variant.name} run tools`,
            project_id: project.id,
            transport: 'streamable_http',
            endpoint: 'http://127.0.0.1:4178/mcp',
            enabled: true,
            ...('unit_price' in variant
              ? { unit_price: variant.unit_price }
              : {})
          },
          undefined,
          201
        );
        const variantAlias = `t_${variantServer.id.replaceAll('-', '')}_${createHash('sha256').update('change').digest('hex').slice(0, 32)}`;
        const variantToolset = await management<Identified>(
          '/api/v1/toolsets',
          'POST',
          {
            project_id: project.id,
            name: `${variant.name} run toolset`,
            toolset: {
              tools: [
                {
                  server_id: variantServer.id,
                  revision_id: variantServer.revision_id,
                  catalog_digest: variantServer.catalog.digest,
                  name: 'change',
                  alias: variantAlias,
                  classification: 'read_only'
                }
              ]
            }
          },
          undefined,
          201
        );
        await management(
          `/api/v1/toolsets/${variantToolset.id}/publish`,
          'POST',
          undefined,
          variantToolset.etag
        );
        return {
          name: variant.name,
          alias: variantAlias,
          toolset: variantToolset
        };
      }
    )
  );
  const createdProvider = await management<Identified>(
    '/api/v1/providers',
    'POST',
    {
      name: 'Run inspection provider',
      project_id: project.id,
      configuration: {
        kind: 'azure_openai',
        auth_mode: 'api_key',
        endpoint: 'http://127.0.0.1:4178',
        deployment: 'vertical-e2e-deployment',
        api_version: '2024-10-21',
        options: {
          models: { 'vertical-e2e-deployment': { context_length: 1000 } }
        }
      },
      model: 'vertical-e2e-deployment',
      credential: 'vertical-provider-secret'
    },
    undefined,
    201
  );
  const providerPath = `/api/v1/providers/${createdProvider.id}`;
  const probe = await management<{ succeeded: boolean }>(
    `${providerPath}/probe`,
    'POST',
    undefined,
    createdProvider.etag
  );
  expect(probe.succeeded).toBe(true);
  const models = await management<{ items: { id: string }[] }>(
    `${providerPath}/models`
  );
  const enabledProvider = await management<Identified>(
    `${providerPath}/models/${models.items[0].id}`,
    'PATCH',
    {
      enabled: true,
      capabilities: [
        { operation: 'generation', surface: 'openai', mode: 'unary' }
      ]
    },
    createdProvider.etag
  );
  const certification = await management<{
    status: string;
    results: { tool_calls: boolean }[];
  }>(
    `${providerPath}/models/${models.items[0].id}/certify`,
    'POST',
    undefined,
    enabledProvider.etag
  );
  expect(certification.status).toBe('certified');
  expect(certification.results[0].tool_calls).toBe(true);
  const certifiedProvider = await management<Identified>(providerPath);
  await management(
    `${providerPath}/activate`,
    'POST',
    undefined,
    certifiedProvider.etag
  );
  const route = 'run-inspection';
  const draft = await management<Identified>(
    '/api/v1/route-drafts',
    'POST',
    {
      project_id: project.id,
      slug: route,
      operations: ['generation'],
      overall_timeout_ms: 10000,
      max_attempts: 1,
      fidelity: { mode: 'transformed' },
      targets: [
        {
          provider_id: createdProvider.id,
          provider_model: 'vertical-e2e-deployment',
          priority: 0,
          weight: 1,
          timeout_ms: 5000
        }
      ],
      tool_execution: 'gateway',
      toolsets: [toolset.id, ...variants.map((variant) => variant.toolset.id)],
      run_limits: {
        max_tool_calls: 32,
        max_duration_seconds: 600,
        max_concurrent_runs: 3
      }
    },
    undefined,
    201
  );
  await management(
    `/api/v1/route-drafts/${draft.id}/activate`,
    'POST',
    undefined,
    draft.etag
  );
  await management(
    '/api/v1/pricing/revisions',
    'POST',
    {
      effective_at: new Date(Date.now() - 3600000).toISOString(),
      prices: [
        {
          provider_kind: 'azure_openai',
          provider_id: createdProvider.id,
          model: 'vertical-e2e-deployment',
          operation: 'generation',
          currency: 'USD',
          input_per_million: '1000000',
          output_per_million: '1000000'
        }
      ]
    },
    undefined,
    201
  );
  const key = await management<{ secret: string }>(
    '/api/v1/api-keys',
    'POST',
    {
      name: 'Run inspection caller',
      project_id: project.id,
      scopes: ['inference', 'tools', 'traces_write'],
      allowed_routes: [route],
      allowed_toolsets: [
        toolset.id,
        ...variants.map((variant) => variant.toolset.id)
      ]
    },
    undefined,
    201
  );

  const inference = {
    model: route,
    messages: [{ role: 'user', content: 'workflow-private-prompt' }],
    max_tokens: 100,
    olp_toolsets: [toolset.id]
  };
  await expect(async () => {
    const catalog = await management<{
      items: { id: string; prices?: { complete: boolean }[] }[];
    }>('/api/v1/catalog');
    const model = catalog.items.find((item) => item.id === route);
    expect(model?.prices?.some((price) => price.complete)).toBe(true);
  }).toPass({ timeout: 30000, intervals: [1000] });

  const pausedResponse = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${key.secret}` },
    data: inference
  });
  expect(pausedResponse.status(), await pausedResponse.text()).toBe(200);
  const paused = (await pausedResponse.json()) as {
    type: string;
    continuation_token: string;
  };
  expect(paused.type).toBe('tool_approval_required');
  const listing = await management<{ items: Run[] }>(
    `/api/v1/agent-runs?project_id=${project.id}`
  );
  const run = listing.items[0];
  const pending = await management<RunDetail>(`/api/v1/agent-runs/${run.id}`);
  expect(pending.approvals).toHaveLength(1);
  expect(pending.approvals[0].consumed_at).toBeNull();

  const resumed = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${key.secret}` },
    data: {
      ...inference,
      olp_approval: {
        continuation_token: paused.continuation_token,
        approved: true,
        arguments: { 'journey-call': { value: 'workflow-private-argument' } }
      }
    }
  });
  expect(resumed.status()).toBe(200);
  const detail = await management<RunDetail>(`/api/v1/agent-runs/${run.id}`);
  expect(detail.steps).toHaveLength(3);
  expect(detail.approvals[0].consumed_at).toBeTruthy();
  const traceFixture = await trace(run.id, detail.steps[0].dispatch_id);
  const traceBatch = Buffer.concat(
    Array.from({ length: 52 }, (_, index) =>
      Buffer.from(
        traceFixture
          .toString('latin1')
          .replace(
            '\x02'.repeat(8),
            Buffer.from(
              (index + 1).toString(16).padStart(16, '0'),
              'hex'
            ).toString('latin1')
          ),
        'latin1'
      )
    )
  );
  const uploaded = await page.request.post('/v1/traces', {
    headers: {
      Authorization: `Bearer ${key.secret}`,
      'Content-Type': 'application/x-protobuf'
    },
    data: traceBatch
  });
  expect(uploaded.status()).toBe(200);
  const newestSpans = await management<{
    items: { id: string; span_id: string }[];
    next_cursor: string | null;
  }>(`/api/v1/agent-runs/${run.id}/spans?limit=50`);
  expect(newestSpans.items).toHaveLength(50);
  expect(newestSpans.next_cursor).toBeTruthy();
  const olderSpans = await management<{
    items: { id: string; span_id: string }[];
  }>(
    `/api/v1/agent-runs/${run.id}/spans?limit=50&cursor=${newestSpans.next_cursor}`
  );
  const oldSpan = olderSpans.items[0];
  expect(oldSpan).toBeDefined();
  expect(newestSpans.items.some((span) => span.id === oldSpan.id)).toBe(false);

  const uncertain = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${key.secret}` },
    data: {
      ...inference,
      messages: [{ role: 'user', content: 'workflow-private-prompt-unknown' }]
    }
  });
  expect(uncertain.status()).toBe(503);
  const updatedListing = await management<{ items: Run[] }>(
    `/api/v1/agent-runs?project_id=${project.id}`
  );
  const unknownRun = updatedListing.items.find((item) => item.id !== run.id);
  expect(unknownRun).toBeDefined();
  if (!unknownRun) throw new Error('Unknown-outcome run was not listed');
  const unknownDetail = await management<RunDetail>(
    `/api/v1/agent-runs/${unknownRun.id}`
  );
  const unknownStep = unknownDetail.steps[0];
  expect(unknownStep.state).toBe('outcome_unknown');
  expect(Number(unknownStep.observed_charge)).toBe(0);
  expect(Number(unknownStep.remaining_charge)).toBeGreaterThan(0);

  const variantRuns = await variants.reduce(
    async (prior, variant) => {
      const completed = await prior;
      const before = await management<{ items: Run[] }>(
        `/api/v1/agent-runs?project_id=${project.id}`
      );
      const response = await page.request.post('/v1/chat/completions', {
        headers: { Authorization: `Bearer ${key.secret}` },
        data: {
          ...inference,
          messages: [
            {
              role: 'user',
              content:
                variant.name === 'Unpriced'
                  ? 'workflow-private-prompt-failed'
                  : 'workflow-private-prompt-free'
            }
          ],
          olp_toolsets: [variant.toolset.id]
        }
      });
      expect(response.status(), await response.text()).toBe(200);
      const after = await management<{ items: Run[] }>(
        `/api/v1/agent-runs?project_id=${project.id}`
      );
      const created = after.items.find(
        (item) => !before.items.some((old) => old.id === item.id)
      );
      expect(created).toBeDefined();
      if (!created) throw new Error('Variant run not found');
      const variantDetail = await management<RunDetail>(
        `/api/v1/agent-runs/${created.id}`
      );
      const tool = variantDetail.steps.find(
        (step) => step.tool_alias === variant.alias
      );
      expect(tool).toMatchObject({
        tool_alias: variant.alias,
        pricing_state: variant.name.toLowerCase(),
        tool_is_error: variant.name === 'Unpriced'
      });
      expect(tool?.observed_cost).toMatch(/^0(?:\.0+)?$/);
      expect(tool?.observed_charge).toMatch(/^0(?:\.0+)?$/);
      expect(JSON.stringify(variantDetail)).not.toContain(
        'workflow-private-failure'
      );
      return [...completed, { ...variant, run: created, tool }];
    },
    Promise.resolve(
      [] as {
        name: string;
        alias: string;
        toolset: Identified;
        run: Run;
        tool: RunDetail['steps'][number] | undefined;
      }[]
    )
  );

  await page
    .getByRole('navigation', { name: 'Primary' })
    .getByRole('link', { name: 'Gateway', exact: true })
    .click();
  await page
    .getByRole('navigation', { name: 'Gateway', exact: true })
    .getByRole('link', { name: 'Runs', exact: true })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Runs', exact: true })
  ).toBeVisible();
  await page.getByLabel('Project').selectOption(project.id);
  await page.getByRole('button', { name: 'Filter' }).click();
  await expect(page.getByRole('link', { name: run.id })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('run-list.png'),
    fullPage: true
  });
  await page.getByRole('link', { name: unknownRun.id }).click();
  await expect(page.getByText('outcome unknown')).toBeVisible();
  await expect(
    page.getByText(
      `${unknownStep.observed_cost} / ${unknownStep.observed_charge} USD`
    )
  ).toBeVisible();
  await expect(
    page.getByText(
      `${unknownStep.remaining_cost} / ${unknownStep.remaining_charge} USD`
    )
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath('run-exposure.png'),
    fullPage: true
  });
  await page.getByRole('link', { name: 'All runs' }).click();
  await page.getByLabel('Project').selectOption(project.id);
  await page.getByRole('button', { name: 'Filter' }).click();
  await page.getByRole('link', { name: run.id }).click();
  await expect(
    page.getByRole('heading', { name: 'Steps and attempts' })
  ).toBeVisible();
  await expect(
    page.getByRole('columnheader', { name: 'Observed cost / charge' })
  ).toBeVisible();
  await expect(
    page.getByRole('columnheader', { name: 'Unresolved cost / charge' })
  ).toBeVisible();
  await expect(page.getByText(detail.approvals[0].id)).toBeVisible();
  await expect(
    page.getByText('01010101010101010101010101010101').first()
  ).toBeVisible();
  await expect(
    page.getByText(detail.steps[0].dispatch_id).first()
  ).toBeVisible();
  for (const privateContent of [
    'workflow-private-prompt',
    'workflow-private-argument',
    'workflow-private-result',
    'workflow-private-span'
  ]) {
    await expect(page.getByText(privateContent)).toHaveCount(0);
  }
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('run-detail.png'),
    fullPage: true
  });
  await page.getByRole('link', { name: 'Save investigation' }).click();
  await expect(page.getByLabel('Run ID')).toHaveValue(run.id);
  await page.getByLabel('From', { exact: true }).fill('2026-01-02T10:30');
  await page.getByLabel('To', { exact: true }).fill('2026-02-03T11:45');
  await page.getByLabel('Minimum duration (ms)').fill('100');
  await page.getByLabel('Maximum duration (ms)').fill('300');
  await page.getByLabel('Error').selectOption('true');
  await page.getByLabel('Tool').fill('change');
  await page.getByLabel('Summary').fill('Investigate the tool outcome');
  await page.getByLabel('Evidence kind').selectOption('approval');
  await page.getByLabel('Evidence ID').fill(detail.approvals[0].id);
  await page.getByRole('button', { name: 'Add evidence' }).click();
  await page.getByLabel('Evidence kind').selectOption('step');
  await page.getByLabel('Evidence ID').fill(detail.steps[1].id);
  await page.getByRole('button', { name: 'Add evidence' }).click();
  await page.getByLabel('Evidence kind').selectOption('span');
  await page.getByLabel('Evidence ID').fill(oldSpan.id);
  await page.getByRole('button', { name: 'Add evidence' }).click();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: info.outputPath('investigation-form.png'),
    fullPage: true
  });
  await page.getByRole('button', { name: 'Save investigation' }).click();
  await expect(
    page.getByRole('heading', { name: 'Investigation', exact: true })
  ).toBeVisible();
  await expect(page.getByText('Investigate the tool outcome')).toBeVisible();
  await expect(page.getByText(detail.approvals[0].id)).toBeVisible();
  await expect(page.getByText('100 ms')).toBeVisible();
  await page.reload();
  const savedFrom = await page.evaluate(() =>
    new Date('2026-01-02T10:30').toLocaleString()
  );
  const savedTo = await page.evaluate(() =>
    new Date('2026-02-03T11:45').toLocaleString()
  );
  await expect(page.getByText(savedFrom, { exact: true })).toBeVisible();
  await expect(page.getByText(savedTo, { exact: true })).toBeVisible();
  await expect(page.getByText('300 ms')).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('investigation-detail.png'),
    fullPage: true
  });
  const investigationID = page.url().split('/').at(-1);
  expect(investigationID).toBeTruthy();
  await page.getByRole('link', { name: 'All investigations' }).click();
  await expect(
    page.getByRole('heading', { name: 'Investigations' })
  ).toBeVisible();
  await expect(
    page.getByRole('link', { name: 'Investigate the tool outcome' })
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('investigations-list.png'),
    fullPage: true
  });
  await page
    .getByRole('link', { name: 'Investigate the tool outcome' })
    .click();
  await page.getByRole('link', { name: detail.steps[1].id }).click();
  const selectedStep = page
    .getByRole('row')
    .filter({ hasText: 'Selected evidence' });
  await expect(selectedStep).toContainText(detail.steps[1].id);
  await expect(selectedStep).toContainText(detail.steps[1].dispatch_id);
  await selectedStep.screenshot({
    path: info.outputPath('investigation-selected-step.png')
  });
  await page.goto(`/investigations/${investigationID}`);
  await page.getByRole('link', { name: oldSpan.id }).click();
  const selectedSpan = page.getByRole('region', { name: 'Selected span' });
  await expect(selectedSpan.getByText(oldSpan.id)).toBeVisible();
  await expect(selectedSpan.getByText(oldSpan.span_id)).toBeVisible();
  await selectedSpan.screenshot({
    path: info.outputPath('investigation-selected-span-detail.png')
  });
  const normalSpans = page.getByRole('region', { name: 'Linked spans' });
  const oldSpanCell = normalSpans
    .getByRole('cell')
    .getByText(oldSpan.span_id, { exact: true });
  await expect(oldSpanCell).toHaveCount(0);
  await normalSpans.getByRole('button', { name: 'Older' }).click();
  await expect(oldSpanCell).toBeVisible();
  await normalSpans.getByRole('button', { name: 'Newest' }).click();
  await expect(oldSpanCell).toHaveCount(0);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('investigation-selected-span.png'),
    fullPage: true
  });
  await page.goto(`/investigations/${investigationID}`);
  await page.getByRole('link', { name: detail.approvals[0].id }).click();
  await expect(
    page.getByRole('heading', { name: 'Run', exact: true })
  ).toBeVisible();
  for (const variant of variantRuns) {
    await page.goto(`/runs/${variant.run.id}`);
    const toolRow = page.getByRole('row').filter({ hasText: variant.alias });
    await expect(toolRow).toBeVisible();
    await expect(toolRow).toContainText(
      variant.name === 'Unpriced' ? 'tool error' : 'succeeded'
    );
    await expect(toolRow).toContainText(
      variant.name === 'Unpriced' ? 'Unknown (unpriced)' : 'Free'
    );
    await expect(page.getByText('workflow-private-failure')).toHaveCount(0);
    if (variant.name === 'Unpriced') {
      expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
      await page.screenshot({
        path: info.outputPath('run-unpriced.png'),
        fullPage: true
      });
    }
  }

  const invitation = await management<{ token: string }>(
    '/api/v1/invitations',
    'POST',
    { email: 'run-viewer@example.com', role: 'viewer' },
    undefined,
    201
  );
  const browser = page.context().browser();
  if (!browser) throw new Error('Browser is unavailable');
  const accepting = await browser.newContext({
    baseURL: 'http://localhost:4175'
  });
  const accepted = await accepting.request.post('/api/v1/invitations/accept', {
    headers: { Origin: 'http://localhost:4175' },
    data: {
      token: invitation.token,
      display_name: 'Run viewer',
      password: 'RunViewerLongPass123!'
    }
  });
  expect(accepted.status()).toBe(201);
  const viewerSession = await accepting.request.get('/api/v1/sessions/current');
  const { user } = (await viewerSession.json()) as { user: { id: string } };
  await accepting.close();
  const viewerRecord = await management<Identified>(`/api/v1/users/${user.id}`);
  await management(
    `/api/v1/users/${user.id}`,
    'PATCH',
    { access_scope: 'assigned' },
    viewerRecord.etag
  );
  const projectBeforeGrant = await management<Identified>(
    `/api/v1/projects/${project.id}`
  );
  await management(
    `/api/v1/projects/${project.id}/members/${user.id}`,
    'PUT',
    { role: 'viewer' },
    projectBeforeGrant.etag
  );
  const viewerContext = await browser.newContext({
    baseURL: 'http://localhost:4175'
  });
  try {
    const loggedIn = await viewerContext.request.post('/api/v1/sessions', {
      headers: { Origin: 'http://localhost:4175' },
      data: {
        email: 'run-viewer@example.com',
        password: 'RunViewerLongPass123!'
      }
    });
    expect(loggedIn.status()).toBe(201);
    const viewerPage = await viewerContext.newPage();
    await viewerPage.goto('/runs');
    await expect(
      viewerPage.getByRole('option', { name: 'Run inspection project' })
    ).toBeAttached();
    await viewerPage.getByRole('link', { name: run.id }).click();
    await expect(viewerPage.getByText(detail.approvals[0].id)).toBeVisible();
    await expect(
      viewerPage.getByText('01010101010101010101010101010101').first()
    ).toBeVisible();
    await viewerPage.goto(`/investigations/${investigationID}`);
    await expect(
      viewerPage.getByText('Investigate the tool outcome')
    ).toBeVisible();
    await viewerPage.goto('/runs');
    await viewerPage.getByRole('link', { name: run.id }).click();
    const listRoute = (url: URL) => url.pathname === '/api/v1/agent-runs';
    await viewerPage.route(listRoute, (route) =>
      route.fulfill({
        status: 403,
        contentType: 'application/problem+json',
        body: JSON.stringify({ title: 'Forbidden', status: 403 })
      })
    );
    await viewerPage.getByRole('link', { name: 'All runs' }).click();
    await expect(
      viewerPage.getByText('Forbidden', { exact: true })
    ).toBeVisible();
    await expect(viewerPage.getByRole('link', { name: run.id })).toHaveCount(0);
    await viewerPage.unroute(listRoute);
    await viewerPage.reload();
    await expect(viewerPage.getByRole('link', { name: run.id })).toBeVisible();
    const detailRoute = `**/api/v1/agent-runs/${run.id}`;
    await viewerPage.route(detailRoute, (route) =>
      route.fulfill({
        status: 403,
        contentType: 'application/problem+json',
        body: JSON.stringify({ title: 'Forbidden', status: 403 })
      })
    );
    await viewerPage.getByRole('link', { name: run.id }).click();
    await expect(viewerPage.getByRole('alert')).toBeVisible();
    await expect(viewerPage.getByText(detail.approvals[0].id)).toHaveCount(0);
    await expect(
      viewerPage.getByText('01010101010101010101010101010101')
    ).toHaveCount(0);
    await viewerPage.unroute(detailRoute);
    await viewerPage.getByRole('link', { name: 'All runs' }).click();
    const spansRoute = `**/api/v1/agent-runs/${run.id}/spans*`;
    await viewerPage.route(spansRoute, (route) =>
      route.fulfill({
        status: 403,
        contentType: 'application/problem+json',
        body: JSON.stringify({ title: 'Forbidden', status: 403 })
      })
    );
    await viewerPage.getByRole('link', { name: run.id }).click();
    await expect(viewerPage.getByText(detail.approvals[0].id)).toBeVisible();
    await expect(viewerPage.getByRole('alert')).toBeVisible();
    await expect(
      viewerPage.getByText('01010101010101010101010101010101')
    ).toHaveCount(0);
    await viewerPage.unroute(spansRoute);
    await viewerPage.getByRole('link', { name: 'All runs' }).click();
    await expect(viewerPage.getByRole('link', { name: run.id })).toBeVisible();
    const projectBeforeRevoke = await management<Identified>(
      `/api/v1/projects/${project.id}`
    );
    const removed = await page.request.delete(
      `/api/v1/projects/${project.id}/members/${user.id}`,
      {
        headers: {
          Origin: 'http://localhost:4175',
          'X-CSRF-Token': csrf,
          'If-Match': `"${projectBeforeRevoke.etag}"`
        }
      }
    );
    expect(removed.status()).toBe(204);
    await viewerPage.goto(`/investigations/${investigationID}`);
    await expect(viewerPage.getByRole('alert')).toBeVisible();
    await expect(
      viewerPage.getByText('Investigate the tool outcome')
    ).toHaveCount(0);
    await viewerPage.goto(`/runs/${run.id}?span_id=${oldSpan.id}`);
    await expect(viewerPage.getByRole('alert')).toBeVisible();
    await expect(viewerPage.getByText(oldSpan.span_id)).toHaveCount(0);
    await expect(viewerPage.getByText(detail.approvals[0].id)).toHaveCount(0);
    await expect(
      viewerPage.getByText('01010101010101010101010101010101')
    ).toHaveCount(0);
    await viewerPage.goto('/runs');
    await expect(viewerPage.getByRole('link', { name: run.id })).toHaveCount(0);
    await expect(
      viewerPage.getByRole('option', { name: 'Run inspection project' })
    ).toHaveCount(0);
  } finally {
    await viewerContext.close();
  }
}
