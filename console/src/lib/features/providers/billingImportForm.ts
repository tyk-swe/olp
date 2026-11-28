import { parseJsonObject } from '#lib/editing/json.js';
import { validated, type FieldErrors } from '#lib/editing/validation.js';
import type {
  AWS_BILLING_AUTH_MODES,
  AZURE_BILLING_AUTH_MODES,
  GOOGLE_BILLING_AUTH_MODES
} from '#lib/management/enums.js';
import type { BillingImport, PutBillingImportInput } from './billing.js';

export type BillingImportFormValues = {
  adminKey: string;
  payerAccount: string;
  bucket: string;
  reportPrefix: string;
  region: string;
  linkedAccounts: string;
  awsAuthMode: (typeof AWS_BILLING_AUTH_MODES)[number];
  accessKeyId: string;
  secretAccessKey: string;
  sessionToken: string;
  replaceCredential: boolean;
  scopeText: string;
  billingScope: string;
  exportName: string;
  storageAccount: string;
  container: string;
  prefix: string;
  authMode: '' | (typeof AZURE_BILLING_AUTH_MODES)[number];
  tenantId: string;
  clientId: string;
  clientSecret: string;
  billingAccountID: string;
  bigQueryProjectID: string;
  datasetID: string;
  tableID: string;
  googleAuthMode: 'keep' | (typeof GOOGLE_BILLING_AUTH_MODES)[number];
  enabled: boolean;
};

export type BillingField = keyof BillingImportFormValues;

export function newBillingImportForm(): BillingImportFormValues {
  return {
    adminKey: '',
    payerAccount: '',
    bucket: '',
    reportPrefix: '',
    region: '',
    linkedAccounts: '',
    awsAuthMode: 'static',
    accessKeyId: '',
    secretAccessKey: '',
    sessionToken: '',
    replaceCredential: false,
    scopeText: '',
    billingScope: '',
    exportName: '',
    storageAccount: '',
    container: '',
    prefix: '',
    authMode: '',
    tenantId: '',
    clientId: '',
    clientSecret: '',
    billingAccountID: '',
    bigQueryProjectID: '',
    datasetID: '',
    tableID: '',
    googleAuthMode: 'service_account',
    enabled: true
  };
}

export function clearBillingImportSecrets(form: BillingImportFormValues): void {
  form.adminKey = '';
  form.payerAccount =
    form.bucket =
    form.reportPrefix =
    form.region =
    form.linkedAccounts =
      '';
  form.accessKeyId = form.secretAccessKey = form.sessionToken = '';
  form.replaceCredential = false;
  form.scopeText = '';
  form.billingScope =
    form.exportName =
    form.storageAccount =
    form.container =
    form.prefix =
      '';
  form.authMode = '';
  form.tenantId = form.clientId = form.clientSecret = '';
  form.billingAccountID =
    form.bigQueryProjectID =
    form.datasetID =
    form.tableID =
      '';
  form.googleAuthMode = 'service_account';
  form.enabled = true;
}

export function scopeLines(document: BillingImport): string {
  const scope = document.scope;
  if ('payer_account_id' in scope) return '';
  if ('billing_scope' in scope)
    return (scope.subscription_ids ?? []).join('\n');

  const entries =
    'project_ids' in scope
      ? (scope.project_ids ?? [])
      : 'workspace_ids' in scope
        ? (scope.workspace_ids ?? [])
        : [];

  return entries.join('\n');
}

export function errorPhrase(code: string): string {
  switch (code) {
    case 'credential':
      return 'the vendor rejected the billing credential';
    case 'status':
      return 'the vendor returned an error';
    case 'network':
      return 'the vendor endpoint was unreachable';
    case 'timeout':
      return 'the vendor did not answer in time';
    case 'response':
      return 'the vendor returned an unexpected response';
    case 'endpoint':
      return 'egress to the endpoint was refused';
    default:
      return code;
  }
}

function lines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);
}

export function scopeBody(
  form: BillingImportFormValues,
  kind: string
): NonNullable<PutBillingImportInput['scope']> {
  if (kind === 'bedrock') {
    const ids = lines(form.linkedAccounts);
    return {
      payer_account_id: form.payerAccount.trim(),
      bucket: form.bucket.trim(),
      report_prefix: form.reportPrefix.trim(),
      region: form.region.trim(),
      ...(ids.length ? { linked_account_ids: ids } : {})
    };
  }
  const ids = lines(form.scopeText);
  if (kind === 'azure_openai') {
    return {
      billing_scope: form.billingScope.trim(),
      export_name: form.exportName.trim(),
      storage_account: form.storageAccount.trim(),
      container: form.container.trim(),
      ...(form.prefix.trim() ? { prefix: form.prefix.trim() } : {}),
      ...(ids.length ? { subscription_ids: ids } : {})
    };
  }
  if (kind === 'vertex_ai') {
    return {
      billing_account_id: form.billingAccountID.trim(),
      bigquery_project_id: form.bigQueryProjectID.trim(),
      dataset_id: form.datasetID.trim(),
      table_id: form.tableID.trim(),
      ...(ids.length ? { project_ids: ids } : {})
    };
  }
  if (!ids.length) return {};
  return kind === 'openai' ? { project_ids: ids } : { workspace_ids: ids };
}

const unless = (met: unknown, message: string) => (met ? undefined : message);

function bedrockProblems(
  form: BillingImportFormValues,
  editing: boolean
): FieldErrors<BillingField> {
  const linked = lines(form.linkedAccounts);
  const keys =
    (!editing || form.replaceCredential) && form.awsAuthMode === 'static';
  return {
    payerAccount: unless(
      /^\d{12}$/.test(form.payerAccount.trim()),
      'Enter the 12-digit payer account ID'
    ),
    bucket: unless(form.bucket.trim(), 'Enter the bucket'),
    reportPrefix: unless(form.reportPrefix.trim(), 'Enter the report prefix'),
    region: unless(form.region.trim(), 'Enter the region'),
    linkedAccounts: unless(
      linked.length <= 100 && linked.every((id) => /^\d{12}$/.test(id)),
      'Enter at most 100 12-digit linked account IDs'
    ),
    accessKeyId: unless(
      !keys || form.accessKeyId.trim(),
      'Enter an access key ID'
    ),
    secretAccessKey: unless(
      !keys || form.secretAccessKey.trim(),
      'Enter a secret access key'
    )
  };
}

function azureProblems(
  form: BillingImportFormValues,
  editing: boolean
): FieldErrors<BillingField> {
  const ids = lines(form.scopeText);
  const secret = form.authMode === 'azure_client_secret';
  return {
    billingScope: unless(
      form.billingScope.trim(),
      'Enter the billing scope resource ID.'
    ),
    exportName: unless(form.exportName.trim(), 'Enter the export name.'),
    storageAccount: unless(
      form.storageAccount.trim(),
      'Enter the storage account.'
    ),
    container: unless(form.container.trim(), 'Enter the container.'),
    scopeText:
      ids.length > 100
        ? 'At most 100 scope IDs can be listed.'
        : unless(
            ids.every((id) =>
              /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
                id
              )
            ),
            'Subscription IDs must be UUIDs.'
          ),
    authMode: unless(
      editing || form.authMode,
      'Choose an Azure credential mode.'
    ),
    tenantId: unless(!secret || form.tenantId.trim(), 'Enter the tenant ID.'),
    clientId: unless(!secret || form.clientId.trim(), 'Enter the client ID.'),
    clientSecret: unless(
      !secret || form.clientSecret,
      'Enter the client secret.'
    )
  };
}

function projectProblems(
  form: BillingImportFormValues,
  kind: string,
  editing: boolean,
  scopePattern: RegExp,
  serviceAccount: Record<string, unknown> | undefined
): FieldErrors<BillingField> {
  const ids = lines(form.scopeText);
  const rejected = ids.find(
    (id) =>
      !(kind === 'anthropic' && id === 'default') && !scopePattern.test(id)
  );
  const scopeText =
    ids.length > 100
      ? 'At most 100 scope IDs can be listed.'
      : rejected &&
        `${rejected} is not a valid ${kind === 'anthropic' ? 'workspace' : 'project'} ID.`;
  if (kind !== 'vertex_ai')
    return {
      scopeText,
      adminKey: unless(
        editing || form.adminKey.trim(),
        'An admin key is required to create the import.'
      )
    };
  const usesKey = form.googleAuthMode === 'service_account';
  return {
    scopeText,
    billingAccountID: unless(
      form.billingAccountID.trim(),
      'Enter the billing account ID.'
    ),
    bigQueryProjectID: unless(
      form.bigQueryProjectID.trim(),
      'Enter the BigQuery project ID.'
    ),
    datasetID: unless(form.datasetID.trim(), 'Enter the dataset ID.'),
    tableID: unless(form.tableID.trim(), 'Enter the detailed export table ID.'),
    adminKey: !usesKey
      ? undefined
      : !form.adminKey.trim()
        ? 'A service account key is required.'
        : unless(
            serviceAccount,
            'The service account key must be a JSON object.'
          )
  };
}

function credentialInput(
  form: BillingImportFormValues,
  kind: string,
  editing: boolean,
  serviceAccount: Record<string, unknown> | undefined
): PutBillingImportInput['credential'] {
  if (kind === 'bedrock') {
    if (editing && !form.replaceCredential) return undefined;
    if (form.awsAuthMode === 'workload') return { auth_mode: 'workload' };
    return {
      auth_mode: 'static',
      access_key_id: form.accessKeyId.trim(),
      secret_access_key: form.secretAccessKey.trim(),
      ...(form.sessionToken.trim()
        ? { session_token: form.sessionToken.trim() }
        : {})
    };
  }
  if (kind === 'azure_openai') {
    if (!form.authMode) return undefined;
    if (form.authMode === 'azure_default') return { auth_mode: form.authMode };
    return {
      auth_mode: form.authMode,
      tenant_id: form.tenantId.trim(),
      client_id: form.clientId.trim(),
      client_secret: form.clientSecret
    };
  }
  if (kind === 'vertex_ai') {
    if (form.googleAuthMode === 'keep') return undefined;
    return form.googleAuthMode === 'adc'
      ? { auth_mode: 'adc' }
      : { auth_mode: 'service_account', service_account: serviceAccount };
  }
  return form.adminKey.trim() || undefined;
}

export function validateBillingImport(
  form: BillingImportFormValues,
  kind: string,
  editing: boolean,
  scopePattern: RegExp
) {
  const serviceAccount = parseJsonObject(form.adminKey) ?? undefined;
  const problems =
    kind === 'bedrock'
      ? bedrockProblems(form, editing)
      : kind === 'azure_openai'
        ? azureProblems(form, editing)
        : projectProblems(form, kind, editing, scopePattern, serviceAccount);
  return validated<PutBillingImportInput, BillingField>(problems, () => {
    const credential = credentialInput(form, kind, editing, serviceAccount);
    return {
      enabled: form.enabled,
      scope: scopeBody(form, kind),
      ...(credential === undefined ? {} : { credential })
    };
  });
}

export function billingImportServerFields(
  form: BillingImportFormValues,
  kind: string
): Readonly<Record<string, BillingField>> {
  const showsAdminKey =
    kind !== 'bedrock' &&
    kind !== 'azure_openai' &&
    (kind !== 'vertex_ai' || form.googleAuthMode === 'service_account');
  return {
    'scope.payer_account_id': 'payerAccount',
    'scope.bucket': 'bucket',
    'scope.report_prefix': 'reportPrefix',
    'scope.region': 'region',
    'scope.linked_account_ids': 'linkedAccounts',
    'scope.billing_scope': 'billingScope',
    'scope.subscription_ids': 'scopeText',
    'scope.project_ids': 'scopeText',
    'scope.workspace_ids': 'scopeText',
    ...(showsAdminKey ? { credential: 'adminKey' as const } : {})
  };
}

export function hydrateBillingImport(
  document: BillingImport | null,
  form: BillingImportFormValues
): void {
  const scope = document?.scope;
  form.scopeText = document ? scopeLines(document) : '';
  const aws = scope && 'payer_account_id' in scope ? scope : null;
  form.payerAccount = aws?.payer_account_id ?? '';
  form.bucket = aws?.bucket ?? '';
  form.reportPrefix = aws?.report_prefix ?? '';
  form.region = aws?.region ?? '';
  form.linkedAccounts = (aws?.linked_account_ids ?? []).join('\n');
  const azureScope = scope && 'billing_scope' in scope ? scope : null;
  form.billingScope = azureScope?.billing_scope ?? '';
  form.exportName = azureScope?.export_name ?? '';
  form.storageAccount = azureScope?.storage_account ?? '';
  form.container = azureScope?.container ?? '';
  form.prefix = azureScope?.prefix ?? '';
  const google = scope && 'billing_account_id' in scope ? scope : null;
  form.billingAccountID = google?.billing_account_id ?? '';
  form.bigQueryProjectID = google?.bigquery_project_id ?? '';
  form.datasetID = google?.dataset_id ?? '';
  form.tableID = google?.table_id ?? '';
  form.googleAuthMode = google ? 'keep' : 'service_account';
  form.enabled = document?.enabled ?? true;
}
