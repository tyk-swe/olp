import { readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { format, resolveConfig } from 'prettier';

const consoleRoot = resolve(import.meta.dirname, '..');
const contract = JSON.parse(
  await readFile(join(consoleRoot, '../management/management.json'), 'utf8')
);
const { schemas } = contract.components;

const billingAuthMode = (credentialProperty) =>
  schemas.PutBillingImportRequest.properties.credential.oneOf.find(
    (variant) => variant.properties?.[credentialProperty]
  )?.properties.auth_mode;

const queryParameter = (path, name) =>
  contract.paths[path].get.parameters.find(
    (parameter) => parameter.in === 'query' && parameter.name === name
  )?.schema;

const enums = {
  ADMISSION_PRIORITIES: schemas.AdmissionPriority,
  AWS_BILLING_AUTH_MODES: billingAuthMode('access_key_id'),
  AZURE_BILLING_AUTH_MODES: billingAuthMode('client_secret'),
  CAPTURE_PARTS: schemas.CapturePolicy.properties.include.items,
  CAPTURE_REDACTOR_TYPES: schemas.CaptureRedactorRevision.properties.type,
  CODE_RESEARCH_SERVICES: schemas.CodeResearchTools.properties.services.items,
  EXTERNAL_CREDENTIAL_STORES:
    schemas.ExternalCredentialReference.properties.store,
  FIXED_ROLES: schemas.ProvisionUserRequest.properties.role,
  GOOGLE_BILLING_AUTH_MODES: billingAuthMode('service_account'),
  GUARDRAIL_ACTIONS: schemas.GuardrailPolicyEntry.properties.action,
  GUARDRAIL_ATTACHMENT_SCOPES: schemas.GuardrailAttachment.properties.scope,
  GUARDRAIL_PHASES: schemas.GuardrailPolicyEntry.properties.phase,
  GUARDRAIL_TYPES: schemas.CreateGuardrailRequest.properties.type,
  PASSTHROUGH_METHODS: schemas.PassthroughConfig.properties.methods.items,
  RETRY_CLASSES: schemas.RouteRetry.propertyNames,
  SIGNATURE_ALGORITHMS: schemas.WorkloadIssuerWrite.properties.algorithms.items,
  SIGNED_SINK_STREAMS: schemas.CreateSinkRequest.properties.streams.items,
  SIMULATION_DIALECTS: schemas.SimulationDialect,
  USAGE_DIMENSIONS: queryParameter('/api/v1/usage/breakdown', 'dimension')
};

function declaration([name, schema]) {
  const values = schema?.enum;
  if (!values?.every((value) => typeof value === 'string')) {
    throw new Error(`${name} is not a string enum in the management contract`);
  }
  return `export const ${name} = ${JSON.stringify(values)} as const;`;
}

const target = join(consoleRoot, 'src/lib/management/enums.ts');
const source = `// Generated from management/management.json by console/scripts/generate-enums.mjs.
// Regenerate with \`make api\`; never edit this file.
${Object.entries(enums).map(declaration).join('\n\n')}
`;
await writeFile(
  target,
  await format(source, {
    ...(await resolveConfig(target)),
    filepath: target
  })
);
