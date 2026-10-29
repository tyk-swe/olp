import { createHash, randomUUID } from 'node:crypto';
import AxeBuilder from '@axe-core/playwright';
import { expect, type Page, type TestInfo } from '@playwright/test';

type Identified = { id: string; etag: string };
type Run = { id: string };
type RunDetail = {
  steps: {
    dispatch_id: string;
    state: string;
    observed_cost: string;
    observed_charge: string;
    remaining_cost: string;
    remaining_charge: string;
  }[];
  approvals: { id: string; consumed_at: string | null }[];
};

function message(field: number, value: Buffer | string): Buffer {
  const payload = typeof value === 'string' ? Buffer.from(value) : value;
  const length: number[] = [];
  let remaining = payload.length;
  do {
    const next = remaining % 128;
    remaining = Math.floor(remaining / 128);
    length.push(next | (remaining ? 128 : 0));
  } while (remaining);
  return Buffer.concat([Buffer.from([(field << 3) | 2, ...length]), payload]);
}

function trace(runID: string, attemptID: string): Buffer {
  const start = BigInt(Date.now()) * 1_000_000n;
  const timestamp = (field: number, value: bigint) => {
    const encoded = Buffer.alloc(9);
    encoded[0] = (field << 3) | 1;
    encoded.writeBigUInt64LE(value, 1);
    return encoded;
  };
  const attribute = (key: string, value: string) =>
    message(9, Buffer.concat([message(1, key), message(2, message(1, value))]));
  const span = Buffer.concat([
    message(1, Buffer.alloc(16, 1)),
    message(2, Buffer.alloc(8, 2)),
    timestamp(7, start),
    timestamp(8, start + 1_000_000n),
    attribute('olp.run_id', runID),
    attribute('olp.attempt_id', attemptID),
    attribute('gen_ai.prompt', 'workflow-private-span')
  ]);
  return message(1, message(2, message(2, span)));
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
  let provider = await management<Identified>(
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
  const providerPath = `/api/v1/providers/${provider.id}`;
  const probe = await management<{ succeeded: boolean }>(
    `${providerPath}/probe`,
    'POST',
    undefined,
    provider.etag
  );
  expect(probe.succeeded).toBe(true);
  const models = await management<{ items: { id: string }[] }>(
    `${providerPath}/models`
  );
  provider = await management<Identified>(
    `${providerPath}/models/${models.items[0].id}`,
    'PATCH',
    {
      enabled: true,
      capabilities: [
        { operation: 'generation', surface: 'openai', mode: 'unary' }
      ]
    },
    provider.etag
  );
  const certification = await management<{
    status: string;
    results: { tool_calls: boolean }[];
  }>(
    `${providerPath}/models/${models.items[0].id}/certify`,
    'POST',
    undefined,
    provider.etag
  );
  expect(certification.status).toBe('certified');
  expect(certification.results[0].tool_calls).toBe(true);
  provider = await management<Identified>(providerPath);
  await management(
    `${providerPath}/activate`,
    'POST',
    undefined,
    provider.etag
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
          provider_id: provider.id,
          provider_model: 'vertical-e2e-deployment',
          priority: 0,
          weight: 1,
          timeout_ms: 5000
        }
      ],
      tool_execution: 'gateway',
      toolsets: [toolset.id],
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
          provider_id: provider.id,
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
      allowed_toolsets: [toolset.id]
    },
    undefined,
    201
  );

  const inference = {
    model: route,
    messages: [{ role: 'user', content: 'workflow-private-prompt' }],
    max_tokens: 100
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
  const uploaded = await page.request.post('/v1/traces', {
    headers: {
      Authorization: `Bearer ${key.secret}`,
      'Content-Type': 'application/x-protobuf'
    },
    data: trace(run.id, detail.steps[0].dispatch_id)
  });
  expect(uploaded.status()).toBe(200);

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
    page.getByText('01010101010101010101010101010101')
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
}
