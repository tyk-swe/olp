package source

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/connectors"
)

const (
	azureMaxManifests  = 1000
	azureMaxFiles      = 200
	azureMaxRows       = 2_000_000
	azureMaxCompressed = 128 << 20
	azureMaxExpanded   = 512 << 20
	azureMaxAggregates = 100_000
	azureTimeout       = time.Minute
)

var azureCostManagement = vendor[azureScope, azureCredential, Cloud]{
	family:       "azure",
	providerKind: connectors.KindAzureOpenAI,
	scope:        parseAzureScope,
	input:        document,
	credential:   parseAzureCredential,
	request:      Inputs.Cloud,
	read:         readAzure,
}

var azureRequiredColumns = []string{"date", "subscriptionid", "metercategory", "metersubcategory", "meterid", "chargetype", "billingcurrency", "costinbillingcurrency"}

type azureManifest struct {
	ByteCount    int64 `json:"byteCount"`
	BlobCount    int   `json:"blobCount"`
	DataRowCount int64 `json:"dataRowCount"`
	ExportConfig struct {
		ExportName string `json:"exportName"`
		ResourceID string `json:"resourceId"`
		Type       string `json:"type"`
	} `json:"exportConfig"`
	DeliveryConfig struct {
		FileFormat      string `json:"fileFormat"`
		CompressionMode string `json:"compressionMode"`
		ContainerURI    string `json:"containerUri"`
	} `json:"deliveryConfig"`
	RunInfo struct {
		SubmittedTime string `json:"submittedTime"`
		RunID         string `json:"runId"`
		StartDate     string `json:"startDate"`
		EndDate       string `json:"endDate"`
	} `json:"runInfo"`
	Blobs []azureBlob `json:"blobs"`
}

type azureBlob struct {
	BlobName     string `json:"blobName"`
	ByteCount    int64  `json:"byteCount"`
	DataRowCount int64  `json:"dataRowCount"`
}

type azureRun struct {
	manifest  azureManifest
	start     time.Time
	end       time.Time
	submitted time.Time
}

type azureTotals struct {
	files      int
	rows       int64
	compressed int64
	expanded   int64
	aggregates int
}

type azureExport struct {
	container *container.Client
	scope     azureScope
	totals    *azureTotals
}

type azureTransport struct {
	client *http.Client
	auth   *connectors.Auth
	mode   connectors.AuthMode
	secret []byte
}

type azureLimitedBody struct {
	io.Reader
	io.Closer
}

func (t azureTransport) Do(req *http.Request) (*http.Response, error) {
	if _, err := t.auth.ApplyAzureStorage(req.Context(), req, t.mode, t.secret); err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if req.URL.Query().Get("comp") == "list" || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body = azureLimitedBody{Reader: io.LimitReader(resp.Body, maxBodyBytes+1), Closer: resp.Body}
	}
	return resp, nil
}

func readAzure(ctx context.Context, req Cloud, scope azureScope, credential azureCredential, emit func(Period) error) (*Resume, error) {
	endpoint := "https://" + scope.StorageAccount + ".blob.core.windows.net"
	client, err := req.Client(endpoint)
	if err != nil {
		return nil, fail(CodeEndpoint, err)
	}
	mode, document := credential.azureAuth()
	secret, err := json.Marshal(document)
	if err != nil {
		return nil, fail(CodeCredential, err)
	}
	ctx, cancel := context.WithTimeout(ctx, azureTimeout)
	defer cancel()
	bounded := *client
	bounded.Timeout = RequestTimeout
	storage, err := azblob.NewClientWithNoCredential(endpoint, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: azureTransport{client: &bounded, auth: req.Auth, mode: mode, secret: secret},
		Retry:     policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		return nil, fail(CodeEndpoint, errors.New("invalid Azure storage endpoint"))
	}
	export := azureExport{container: storage.ServiceClient().NewContainerClient(scope.Container), scope: scope, totals: &azureTotals{}}
	pending := pendingMonths(req)
	names, err := export.list(ctx, pending)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fail(CodeResponse, errors.New("no Azure export manifests found"))
	}
	inMonth := func(month time.Time) func(string) bool {
		return func(name string) bool { return strings.HasPrefix(name, export.monthPrefix(month)) }
	}
	months := slices.DeleteFunc(slices.Clone(pending), func(month time.Time) bool { return !slices.ContainsFunc(names, inMonth(month)) })
	return walkMonths(req, months, len(months), func(month time.Time) error {
		return export.month(ctx, req, slices.DeleteFunc(slices.Clone(names), func(name string) bool { return !inMonth(month)(name) }), emit)
	})
}

func (e azureExport) month(ctx context.Context, req Cloud, names []string, emit func(Period) error) error {
	runs, err := e.runs(ctx, names)
	if err != nil {
		return err
	}
	for index, run := range runs {
		days, err := e.read(ctx, req, run)
		if errors.Is(err, errMissing) && index+1 < len(runs) {
			continue
		}
		if err != nil {
			return err
		}
		e.totals.aggregates += days.lineCount()
		return emit(Period{Days: days, Revision: &Revision{PeriodStart: day(run.start), ID: run.manifest.RunInfo.RunID, OrderingAt: run.submitted}})
	}
	return nil
}

func (e azureExport) monthPrefix(month time.Time) string {
	return strings.Trim(strings.Join([]string{e.scope.Prefix, e.scope.ExportName}, "/"), "/") + "/" + month.Format("20060102") + "-"
}

func (e azureExport) list(ctx context.Context, months []time.Time) ([]string, error) {
	names := []string{}
	for _, month := range months {
		prefix := e.monthPrefix(month)
		pager := e.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
		for pages := 0; pager.More(); pages++ {
			if pages >= maxPages {
				return nil, fail(CodeResponse, errors.New("too many listing pages"))
			}
			result, err := pager.NextPage(ctx)
			if err != nil {
				return nil, storageFailure(ctx, err)
			}
			if result.NextMarker != nil && len(*result.NextMarker) > 2048 {
				return nil, fail(CodeResponse, errors.New("invalid listing continuation"))
			}
			if result.Segment == nil || slices.ContainsFunc(result.Segment.BlobItems, func(blob *container.BlobItem) bool { return blob == nil || blob.Name == nil }) {
				return nil, fail(CodeResponse, errors.New("invalid Azure listing"))
			}
			for _, blob := range result.Segment.BlobItems {
				name := *blob.Name
				if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(strings.ToLower(name), "manifest.json") {
					continue
				}
				if !validAzurePath(name, true) || len(names) >= azureMaxManifests {
					return nil, fail(CodeResponse, errors.New("too many or invalid export manifests"))
				}
				names = append(names, name)
			}
		}
	}
	return names, nil
}

func (e azureExport) runs(ctx context.Context, names []string) ([]azureRun, error) {
	runs := make([]azureRun, 0, len(names))
	for _, name := range names {
		run, err := e.manifest(ctx, name)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	slices.SortFunc(runs, func(a, b azureRun) int { return b.submitted.Compare(a.submitted) })
	return runs, nil
}

func azureTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02", "01/02/2006"} {
		if date, err := time.Parse(layout, value); err == nil {
			return date.UTC(), nil
		}
	}
	return time.Time{}, errors.New("invalid Azure date")
}

func (e azureExport) document(ctx context.Context, name string) (azureManifest, error) {
	response, err := e.container.NewBlobClient(name).DownloadStream(ctx, nil)
	if err != nil {
		return azureManifest{}, storageFailure(ctx, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return azureManifest{}, storageFailure(ctx, err)
	}
	var manifest azureManifest
	if len(data) > maxBodyBytes || json.Unmarshal(data, &manifest) != nil {
		return azureManifest{}, fail(CodeResponse, errors.New("invalid or oversized Azure manifest"))
	}
	return manifest, nil
}

func (e azureExport) manifest(ctx context.Context, name string) (azureRun, error) {
	manifest, err := e.document(ctx, name)
	if err != nil {
		return azureRun{}, err
	}
	start, startErr := azureTime(manifest.RunInfo.StartDate)
	end, endErr := azureTime(manifest.RunInfo.EndDate)
	submitted, submittedErr := azureTime(manifest.RunInfo.SubmittedTime)
	resourceID := strings.TrimRight(e.scope.BillingScope, "/") + "/providers/Microsoft.CostManagement/exports/" + e.scope.ExportName
	if !strings.HasPrefix(name, e.monthPrefix(start)) || !strings.EqualFold(manifest.ExportConfig.ResourceID, resourceID) || !strings.EqualFold(manifest.ExportConfig.ExportName, e.scope.ExportName) ||
		!strings.EqualFold(manifest.ExportConfig.Type, "AmortizedCost") || !strings.EqualFold(manifest.DeliveryConfig.FileFormat, "Csv") ||
		!strings.EqualFold(manifest.DeliveryConfig.CompressionMode, "None") && !strings.EqualFold(manifest.DeliveryConfig.CompressionMode, "Gzip") ||
		startErr != nil || endErr != nil || submittedErr != nil || manifest.RunInfo.RunID == "" || len(manifest.RunInfo.RunID) > 256 ||
		start.Day() != 1 || end.Before(start) || end.After(start.AddDate(0, 1, 0)) || manifest.BlobCount != len(manifest.Blobs) ||
		manifest.BlobCount > azureMaxFiles || manifest.ByteCount < 0 || manifest.ByteCount > azureMaxCompressed || manifest.DataRowCount < 0 || manifest.DataRowCount > azureMaxRows {
		return azureRun{}, fail(CodeResponse, errors.New("invalid or unsupported Azure export manifest"))
	}
	if uri := manifest.DeliveryConfig.ContainerURI; uri != "" && (!strings.EqualFold(path.Base(uri), e.scope.StorageAccount) || !strings.EqualFold(path.Base(path.Dir(uri)), "storageAccounts")) {
		return azureRun{}, fail(CodeResponse, errors.New("export storage account does not match"))
	}
	if slices.ContainsFunc(manifest.Blobs, func(blob azureBlob) bool {
		return !strings.HasPrefix(blob.BlobName, path.Dir(name)+"/") || !validAzurePath(blob.BlobName, true) || blob.ByteCount < 0 || blob.ByteCount > azureMaxCompressed || blob.DataRowCount < 0 || blob.DataRowCount > azureMaxRows
	}) {
		return azureRun{}, fail(CodeResponse, errors.New("invalid export blob"))
	}
	if err := azureBlobTotals(manifest); err != nil {
		return azureRun{}, err
	}
	return azureRun{manifest: manifest, start: start, end: end, submitted: submitted}, nil
}

func azureBlobTotals(manifest azureManifest) error {
	bytes, rows := int64(0), int64(0)
	for _, blob := range manifest.Blobs {
		bytes += blob.ByteCount
		rows += blob.DataRowCount
		if bytes > azureMaxCompressed || rows > azureMaxRows {
			return fail(CodeResponse, errors.New("export exceeds its limits"))
		}
	}
	if bytes != manifest.ByteCount || rows != manifest.DataRowCount {
		return fail(CodeResponse, errors.New("export counts do not match"))
	}
	return nil
}

func (e azureExport) read(ctx context.Context, req Cloud, run azureRun) (Days, error) {
	if e.totals.files+run.manifest.BlobCount > azureMaxFiles || e.totals.compressed+run.manifest.ByteCount > azureMaxCompressed || e.totals.rows+run.manifest.DataRowCount > azureMaxRows {
		return Days{}, fail(CodeResponse, errBudget)
	}
	days := newDays()
	last := slices.MinFunc([]time.Time{day(run.end), req.Today.AddDate(0, 0, -1), run.start.AddDate(0, 1, -1), day(run.submitted).AddDate(0, 0, -1)}, time.Time.Compare)
	first := slices.MaxFunc([]time.Time{day(run.start), req.Earliest}, time.Time.Compare)
	for t := first; !t.After(last); t = t.AddDate(0, 0, 1) {
		days.include(t)
	}
	for _, blob := range run.manifest.Blobs {
		if err := e.blob(ctx, run, blob, days); err != nil {
			return Days{}, err
		}
	}
	return days, nil
}

func (e azureExport) blob(ctx context.Context, run azureRun, blob azureBlob, days Days) error {
	e.totals.files++
	resp, err := e.container.NewBlobClient(blob.BlobName).DownloadStream(ctx, nil)
	if err != nil {
		return storageFailure(ctx, err)
	}
	defer resp.Body.Close()
	limited := &io.LimitedReader{R: resp.Body, N: azureMaxCompressed - e.totals.compressed + 1}
	contents, err := azureContents(limited, run.manifest.DeliveryConfig.CompressionMode)
	if err != nil {
		e.totals.compressed = azureMaxCompressed + 1 - limited.N
		if limited.N == 0 {
			return fail(CodeResponse, errBudget)
		}
		return fail(CodeResponse, errors.New("invalid compressed export"))
	}
	defer contents.Close()
	decoded := &io.LimitedReader{R: contents, N: azureMaxExpanded - e.totals.expanded + 1}
	count, err := e.csv(decoded, days, azureMaxRows-e.totals.rows)
	readBytes := azureMaxCompressed - e.totals.compressed + 1 - limited.N
	e.totals.rows += count
	e.totals.compressed += readBytes
	e.totals.expanded += azureMaxExpanded - e.totals.expanded + 1 - decoded.N
	if limited.N == 0 || decoded.N == 0 || errors.Is(err, errBudget) {
		return fail(CodeResponse, errBudget)
	}
	if timeout, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || ok && timeout.Timeout() {
		return storageFailure(ctx, err)
	}
	if err != nil || readBytes != blob.ByteCount || count != blob.DataRowCount || e.totals.compressed > azureMaxCompressed || e.totals.expanded > azureMaxExpanded || e.totals.rows > azureMaxRows {
		return fail(CodeResponse, errors.New("invalid, incomplete, or oversized export data"))
	}
	return nil
}

func azureContents(body io.Reader, compression string) (io.ReadCloser, error) {
	if strings.EqualFold(compression, "Gzip") {
		return gzip.NewReader(body)
	}
	return io.NopCloser(body), nil
}

func (e azureExport) csv(input io.Reader, days Days, rowLimit int64) (int64, error) {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return 0, err
	}
	fields := make(map[string]int, len(header))
	for index, name := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))
		if _, exists := fields[key]; exists {
			return 0, errors.New("duplicate export column")
		}
		fields[key] = index
	}
	if index, ok := fields["billingcurrencycode"]; ok {
		fields["billingcurrency"] = index
	}
	if slices.ContainsFunc(azureRequiredColumns, func(name string) bool { _, ok := fields[name]; return !ok }) {
		return 0, errors.New("required export column missing")
	}
	for count := int64(0); ; count++ {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if count >= rowLimit && err == nil {
			return count, errBudget
		}
		if err != nil {
			return count, errors.New("invalid export row")
		}
		if err := e.add(days, func(name string) string { return row[fields[name]] }); err != nil {
			return count + 1, err
		}
	}
}

func (e azureExport) add(days Days, field func(string) string) error {
	category, subcategory := field("metercategory"), field("metersubcategory")
	if !strings.EqualFold(category, "Cognitive Services") || !strings.EqualFold(subcategory, "Azure Open AI") && !strings.EqualFold(subcategory, "Azure Open AI Reservation") ||
		len(e.scope.SubscriptionIDs) > 0 && !slices.Contains(e.scope.SubscriptionIDs, strings.ToLower(field("subscriptionid"))) ||
		strings.EqualFold(field("chargetype"), "Tax") {
		return nil
	}
	date, err := azureTime(field("date"))
	if err != nil {
		return err
	}
	if !days.has(date) {
		return nil
	}
	if field("meterid") == "" {
		return errors.New("invalid Azure meter")
	}
	amount, err := decimal.NewFromString(field("costinbillingcurrency"))
	if err != nil {
		return err
	}
	before := days.size(date)
	if err := days.add(date, fmt.Sprintf("Azure OpenAI / %s / %s", field("meterid"), field("chargetype")), field("billingcurrency"), amount); err != nil {
		return err
	}
	if days.size(date) > before && e.totals.aggregates+days.lineCount() > azureMaxAggregates {
		return errBudget
	}
	return nil
}
