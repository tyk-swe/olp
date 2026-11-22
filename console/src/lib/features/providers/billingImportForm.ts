import type { BillingImport, PutBillingImportInput } from './billing.js';

export type BillingImportFormValues = {
  adminKey: string;
  payerAccount: string;
  bucket: string;
  reportPrefix: string;
  region: string;
  linkedAccounts: string;
  awsAuthMode: 'static' | 'workload';
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
  authMode: '' | 'azure_default' | 'azure_client_secret';
  tenantId: string;
  clientId: string;
  clientSecret: string;
  billingAccountID: string;
  bigQueryProjectID: string;
  datasetID: string;
  tableID: string;
  googleAuthMode: 'keep' | 'service_account' | 'adc';
  enabled: boolean;
};

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

export function scopeBody(
  form: BillingImportFormValues,
  kind: string
): NonNullable<PutBillingImportInput['scope']> {
  if (kind === 'bedrock') {
    const ids = form.linkedAccounts
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    return {
      payer_account_id: form.payerAccount.trim(),
      bucket: form.bucket.trim(),
      report_prefix: form.reportPrefix.trim(),
      region: form.region.trim(),
      ...(ids.length ? { linked_account_ids: ids } : {})
    };
  }
  const ids = form.scopeText
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);
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

export function validateForm(
  form: BillingImportFormValues,
  kind: string,
  editing: boolean,
  scopePattern: RegExp,
  serviceAccountKey: Record<string, unknown> | undefined
): string {
  if (kind === 'bedrock') {
    if (!/^\d{12}$/.test(form.payerAccount.trim()))
      return 'Enter the 12-digit payer account ID';
    if (!form.bucket.trim() || !form.reportPrefix.trim() || !form.region.trim())
      return 'Enter the bucket, report prefix, and region';

    const ids = form.linkedAccounts
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);

    if (ids.length > 100 || ids.some((id) => !/^\d{12}$/.test(id)))
      return 'Enter at most 100 12-digit linked account IDs';
    if (
      (!editing || form.replaceCredential) &&
      form.awsAuthMode === 'static' &&
      (!form.accessKeyId.trim() || !form.secretAccessKey.trim())
    )
      return 'Enter an access key ID and secret access key';

    return '';
  }
  const ids = form.scopeText
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);
  if (ids.length > 100) return 'At most 100 scope IDs can be listed.';
  if (kind === 'azure_openai') {
    if (
      !form.billingScope.trim() ||
      !form.exportName.trim() ||
      !form.storageAccount.trim() ||
      !form.container.trim()
    )
      return 'Billing scope, export name, storage account, and container are required.';
    if (
      ids.some(
        (id) =>
          !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
            id
          )
      )
    )
      return 'Subscription IDs must be UUIDs.';
    if (!editing && !form.authMode) return 'Choose an Azure credential mode.';
    if (
      form.authMode === 'azure_client_secret' &&
      (!form.tenantId.trim() || !form.clientId.trim() || !form.clientSecret)
    )
      return 'Tenant ID, client ID, and client secret are required.';
    return '';
  }
  for (const id of ids) {
    if (kind === 'anthropic' && id === 'default') continue;
    if (!scopePattern.test(id))
      return `${id} is not a valid ${kind === 'anthropic' ? 'workspace' : 'project'} ID.`;
  }
  if (kind === 'vertex_ai') {
    if (
      !form.billingAccountID.trim() ||
      !form.bigQueryProjectID.trim() ||
      !form.datasetID.trim() ||
      !form.tableID.trim()
    )
      return 'A billing account, BigQuery project, dataset and table are required.';
    if (form.googleAuthMode === 'service_account' && !form.adminKey.trim())
      return 'A service account key is required.';
    if (form.googleAuthMode === 'service_account' && !serviceAccountKey)
      return 'The service account key must be a JSON object.';
  } else if (!editing && !form.adminKey.trim())
    return 'An admin key is required to create the import.';
  return '';
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
