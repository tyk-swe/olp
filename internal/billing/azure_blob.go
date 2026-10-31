package billing

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/tyk-swe/olp/internal/egress"
)

const (
	azureMaxManifests  = 1000
	azureMaxFiles      = 200
	azureMaxRows       = 2_000_000
	azureMaxCompressed = 128 << 20
	azureMaxExpanded   = 512 << 20
	azureMaxAggregates = 100_000
	azureImportTimeout = time.Minute
)

var azureMissing = errors.New("blob missing")

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
	Blobs []struct {
		BlobName     string `json:"blobName"`
		ByteCount    int64  `json:"byteCount"`
		DataRowCount int64  `json:"dataRowCount"`
	} `json:"blobs"`
}

type azureRun struct {
	manifest  azureManifest
	name      string
	start     time.Time
	end       time.Time
	submitted time.Time
}

type azurePeriod struct {
	start    string
	revision sourceRevision
	days     reportAccumulator
}

type azureReader struct {
	container *container.Client
	scope     azureScope
}

type azureTransport struct {
	client     *http.Client
	auth       *connectors.Auth
	credential azureCredential
	secret     []byte
}

type azureLimitedBody struct {
	io.Reader
	io.Closer
}

func (t *azureTransport) Do(req *http.Request) (*http.Response, error) {
	if _, err := t.auth.ApplyAzureStorage(req.Context(), req, t.credential.AuthMode, t.secret); err != nil {
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

func azureSDKError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return failFetch("timeout", errors.New("Azure storage request timed out"))
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return failFetch("timeout", errors.New("Azure storage request timed out"))
	}
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		switch response.StatusCode {
		case http.StatusNotFound:
			return failFetch("status", azureMissing)
		case http.StatusUnauthorized, http.StatusForbidden:
			return failFetch("credential", errors.New("Azure storage rejected the credential"))
		default:
			return failFetch("status", errors.New("Azure storage request failed"))
		}
	}
	if errors.Is(err, connectors.ErrAuthentication) || errors.Is(err, connectors.ErrCredentialRejected) {
		return failFetch("credential", errors.New("Azure storage authentication failed"))
	}
	return failFetch("network", errors.New("Azure storage request failed"))
}

func fetchAzure(ctx context.Context, client *http.Client, egressPolicy *egress.Policy, endpoint string, scope azureScope, key []byte) ([]azurePeriod, error) {
	var credential azureCredential
	if err := json.Unmarshal(key, &credential); err != nil {
		return nil, failFetch("credential", errors.New("invalid credential"))
	}
	secret, err := json.Marshal(struct {
		TenantID     string `json:"tenant_id"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}{credential.TenantID, credential.ClientID, credential.ClientSecret})
	if err != nil {
		return nil, failFetch("credential", err)
	}
	defer clear(secret)
	ctx, cancel := context.WithTimeout(ctx, azureImportTimeout)
	defer cancel()
	boundedClient := *client
	boundedClient.Timeout = requestTimout
	transport := &azureTransport{client: &boundedClient, auth: connectors.NewAuth(egressPolicy), credential: credential, secret: secret}
	storage, err := azblob.NewClientWithNoCredential(endpoint, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		return nil, failFetch("endpoint", errors.New("invalid Azure storage endpoint"))
	}
	reader := azureReader{container: storage.ServiceClient().NewContainerClient(scope.Container), scope: scope}
	names, err := reader.list(ctx)
	if err != nil {
		return nil, err
	}
	runs := map[string][]azureRun{}
	for _, name := range names {
		run, err := reader.manifest(ctx, name)
		if err != nil {
			return nil, err
		}
		period := run.start.Format(dayFormat)
		runs[period] = append(runs[period], run)
	}
	periods := make([]azurePeriod, 0, len(runs))
	var rows, compressed, expanded int64
	var aggregates int
	for _, period := range slices.Sorted(maps.Keys(runs)) {
		candidates := runs[period]
		slices.SortFunc(candidates, func(a, b azureRun) int { return b.submitted.Compare(a.submitted) })
		for index, run := range candidates {
			days, err := reader.readRun(ctx, run, &rows, &compressed, &expanded)
			if errors.Is(err, azureMissing) && index+1 < len(candidates) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, lines := range days {
				aggregates += len(lines)
			}
			if aggregates > azureMaxAggregates {
				return nil, failFetch("response", errors.New("too many aggregates"))
			}
			periods = append(periods, azurePeriod{start: period, revision: sourceRevision{id: run.manifest.RunInfo.RunID, orderingAt: run.submitted}, days: days})
			break
		}
	}
	return periods, nil
}

func (a *azureReader) document(ctx context.Context, name string, out any) error {
	response, err := a.container.NewBlobClient(name).DownloadStream(ctx, nil)
	if err != nil {
		return azureSDKError(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return azureSDKError(err)
	}
	if len(data) > maxBodyBytes || json.Unmarshal(data, out) != nil {
		return failFetch("response", errors.New("invalid or oversized Azure manifest"))
	}
	return nil
}

func (a *azureReader) list(ctx context.Context) ([]string, error) {
	prefix := strings.Trim(strings.Join([]string{a.scope.Prefix, a.scope.ExportName}, "/"), "/") + "/"
	pager := a.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
	names := []string{}
	for page := 0; pager.More(); page++ {
		if page >= maxPages {
			return nil, failFetch("response", errors.New("too many listing pages"))
		}
		result, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureSDKError(err)
		}
		if result.NextMarker != nil && len(*result.NextMarker) > 2048 {
			return nil, failFetch("response", errors.New("invalid listing continuation"))
		}
		if result.Segment == nil {
			return nil, failFetch("response", errors.New("invalid Azure listing"))
		}
		for _, blob := range result.Segment.BlobItems {
			if blob == nil || blob.Name == nil {
				return nil, failFetch("response", errors.New("invalid Azure listing"))
			}
			name := *blob.Name
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(strings.ToLower(name), "manifest.json") {
				if !validAzurePath(name, true) || len(names) >= azureMaxManifests {
					return nil, failFetch("response", errors.New("too many or invalid export manifests"))
				}
				names = append(names, name)
			}
		}
	}
	return names, nil
}

func azureTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if date, err := time.Parse(layout, value); err == nil {
			return date.UTC(), nil
		}
	}
	return time.Time{}, errors.New("invalid Azure date")
}

func (a *azureReader) manifest(ctx context.Context, name string) (azureRun, error) {
	var manifest azureManifest
	if err := a.document(ctx, name, &manifest); err != nil {
		return azureRun{}, err
	}
	start, startErr := azureTime(manifest.RunInfo.StartDate)
	end, endErr := azureTime(manifest.RunInfo.EndDate)
	submitted, submittedErr := azureTime(manifest.RunInfo.SubmittedTime)
	resourceID := strings.TrimRight(a.scope.BillingScope, "/") + "/providers/Microsoft.CostManagement/exports/" + a.scope.ExportName
	if !strings.EqualFold(manifest.ExportConfig.ResourceID, resourceID) || !strings.EqualFold(manifest.ExportConfig.ExportName, a.scope.ExportName) ||
		!strings.EqualFold(manifest.ExportConfig.Type, "AmortizedCost") || !strings.EqualFold(manifest.DeliveryConfig.FileFormat, "Csv") ||
		(!strings.EqualFold(manifest.DeliveryConfig.CompressionMode, "None") && !strings.EqualFold(manifest.DeliveryConfig.CompressionMode, "Gzip")) ||
		startErr != nil || endErr != nil || submittedErr != nil || manifest.RunInfo.RunID == "" || len(manifest.RunInfo.RunID) > 256 ||
		start.Day() != 1 || end.Before(start) || end.After(start.AddDate(0, 1, 0)) || manifest.BlobCount != len(manifest.Blobs) ||
		manifest.BlobCount > azureMaxFiles || manifest.ByteCount < 0 || manifest.ByteCount > azureMaxCompressed || manifest.DataRowCount < 0 || manifest.DataRowCount > azureMaxRows {
		return azureRun{}, failFetch("response", errors.New("invalid or unsupported Azure export manifest"))
	}
	if uri := manifest.DeliveryConfig.ContainerURI; uri != "" && (!strings.EqualFold(path.Base(uri), a.scope.StorageAccount) || !strings.EqualFold(path.Base(path.Dir(uri)), "storageAccounts")) {
		return azureRun{}, failFetch("response", errors.New("export storage account does not match"))
	}
	var bytes, rows int64
	for _, blob := range manifest.Blobs {
		if !strings.HasPrefix(blob.BlobName, path.Dir(name)+"/") || !validAzurePath(blob.BlobName, true) || blob.ByteCount < 0 || blob.ByteCount > azureMaxCompressed || blob.DataRowCount < 0 || blob.DataRowCount > azureMaxRows {
			return azureRun{}, failFetch("response", errors.New("invalid export blob"))
		}
		bytes += blob.ByteCount
		rows += blob.DataRowCount
		if bytes > azureMaxCompressed || rows > azureMaxRows {
			return azureRun{}, failFetch("response", errors.New("export exceeds its limits"))
		}
	}
	if bytes != manifest.ByteCount || rows != manifest.DataRowCount {
		return azureRun{}, failFetch("response", errors.New("export counts do not match"))
	}
	return azureRun{manifest: manifest, name: name, start: start, end: end, submitted: submitted}, nil
}

func (a *azureReader) readRun(ctx context.Context, run azureRun, rows, compressed, expanded *int64) (reportAccumulator, error) {
	days := reportAccumulator{}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	last := slices.MinFunc([]time.Time{run.end.Truncate(24 * time.Hour), today.AddDate(0, 0, -1), run.start.AddDate(0, 1, -1)}, time.Time.Compare)
	for day := run.start.Truncate(24 * time.Hour); !day.After(last); day = day.AddDate(0, 0, 1) {
		days.ensureDay(day.Format(dayFormat))
	}
	for _, blob := range run.manifest.Blobs {
		resp, err := a.container.NewBlobClient(blob.BlobName).DownloadStream(ctx, nil)
		if err != nil {
			return nil, azureSDKError(err)
		}
		limited := &io.LimitedReader{R: resp.Body, N: azureMaxCompressed - *compressed + 1}
		var contents io.Reader = limited
		if strings.EqualFold(run.manifest.DeliveryConfig.CompressionMode, "Gzip") {
			zipped, err := gzip.NewReader(limited)
			if err != nil {
				resp.Body.Close()
				return nil, failFetch("response", errors.New("invalid compressed export"))
			}
			defer zipped.Close()
			contents = zipped
		}
		decoded := &io.LimitedReader{R: contents, N: azureMaxExpanded - *expanded + 1}
		count, err := a.csv(decoded, days, run.start, last)
		resp.Body.Close()
		readBytes := azureMaxCompressed - *compressed + 1 - limited.N
		readExpanded := azureMaxExpanded - *expanded + 1 - decoded.N
		*compressed += readBytes
		*expanded += readExpanded
		*rows += count
		if err != nil {
			var timeout net.Error
			if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
				return nil, azureSDKError(err)
			}
		}
		if err != nil || readBytes != blob.ByteCount || count != blob.DataRowCount || *compressed > azureMaxCompressed || *expanded > azureMaxExpanded || *rows > azureMaxRows {
			return nil, failFetch("response", errors.New("invalid, incomplete, or oversized export data"))
		}
	}
	return days, nil
}

func (a *azureReader) csv(input io.Reader, days reportAccumulator, start, end time.Time) (int64, error) {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return 0, err
	}
	fields := map[string]int{}
	for index, name := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))
		if _, exists := fields[key]; exists {
			return 0, errors.New("duplicate export column")
		}
		fields[key] = index
	}
	for _, name := range []string{"date", "subscriptionid", "metercategory", "metersubcategory", "meterid", "chargetype", "billingcurrency", "costinbillingcurrency"} {
		if _, ok := fields[name]; !ok {
			return 0, errors.New("required export column missing")
		}
	}
	var count int64
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			var aggregates int
			for _, lines := range days {
				aggregates += len(lines)
			}
			if aggregates > azureMaxAggregates {
				return count, errors.New("too many aggregates")
			}
			return count, nil
		}
		if err != nil || count >= azureMaxRows {
			return count, errors.New("invalid export row")
		}
		count++
		category, subcategory := row[fields["metercategory"]], row[fields["metersubcategory"]]
		if !strings.EqualFold(category, "Cognitive Services") ||
			(!strings.EqualFold(subcategory, "Azure Open AI") && !strings.EqualFold(subcategory, "Azure Open AI Reservation")) {
			continue
		}
		if len(a.scope.SubscriptionIDs) > 0 && !slices.Contains(a.scope.SubscriptionIDs, strings.ToLower(row[fields["subscriptionid"]])) {
			continue
		}
		if strings.EqualFold(row[fields["chargetype"]], "Tax") {
			continue
		}
		day, err := azureTime(row[fields["date"]])
		if err != nil {
			return count, err
		}
		day = day.Truncate(24 * time.Hour)
		if day.Before(start) || day.After(end) {
			continue
		}
		meter, currency := row[fields["meterid"]], row[fields["billingcurrency"]]
		if meter == "" || !validAzureCurrency(currency) {
			return count, errors.New("invalid Azure meter or currency")
		}
		amount, err := decimal.NewFromString(row[fields["costinbillingcurrency"]])
		if err != nil {
			return count, err
		}
		label := azureLabel(fmt.Sprintf("Azure OpenAI / %s / %s", meter, row[fields["chargetype"]]))
		days.add(day.Format(dayFormat), amount, currency, &label)
		if count%1000 == 0 {
			var aggregates int
			for _, lines := range days {
				aggregates += len(lines)
			}
			if aggregates > azureMaxAggregates {
				return count, errors.New("too many aggregates")
			}
		}
	}
}

func validAzureCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, c := range currency {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func azureLabel(value string) string {
	runes := []rune(value)
	if len(runes) <= 256 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return string(runes[:191]) + "-" + hex.EncodeToString(digest[:])
}
