package source

import (
	"archive/zip"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/shopspring/decimal"
)

const (
	awsTimeout       = 45 * time.Second
	awsMaxFiles      = 1000
	awsMaxRows       = 2000000
	awsMaxKeys       = 100000
	awsMaxCompressed = 64 << 20
	awsMaxExpanded   = 512 << 20
	awsManifestTime  = "20060102T150405.000Z"
)

var awsCUR = vendor[awsScope, awsCredential, Cloud]{
	family:       "aws",
	providerKind: "bedrock",
	scope:        parseAWSScope,
	input:        document,
	credential:   parseAWSCredential,
	request:      Inputs.Cloud,
	read:         readAWS,
}

var awsSkippedLineTypes = []string{"Tax", "SavingsPlanNegation", "SavingsPlanRecurringFee", "SavingsPlanUpfrontFee", "RIFee", "Fee"}

var awsRequiredColumns = []string{"bill/PayerAccountId", "lineItem/UsageAccountId", "lineItem/ProductCode", "lineItem/UsageStartDate", "lineItem/LineItemType", "lineItem/UsageType", "lineItem/Operation", "product/sku", "lineItem/CurrencyCode", "lineItem/UnblendedCost"}

type awsManifest struct {
	AssemblyID    string `json:"assemblyId"`
	BillingPeriod struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"billingPeriod"`
	ReportKeys []string `json:"reportKeys"`
}

type awsDelivery struct {
	key      string
	modified time.Time
	month    time.Time
}

type awsBudget struct {
	compressed int64
	expanded   int64
}

type awsReport struct {
	client *s3.Client
	scope  awsScope
	budget *awsBudget
}

func readAWS(ctx context.Context, req Cloud, scope awsScope, credential awsCredential, emit func(Period) error) (*Resume, error) {
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	client, err := awsClient(ctx, req, scope, credential)
	if err != nil {
		return nil, err
	}
	report := awsReport{client: client, scope: scope, budget: &awsBudget{}}
	deliveries, err := report.deliveries(ctx, pendingMonths(req))
	if err != nil {
		return nil, err
	}
	if len(deliveries) == 0 {
		return nil, fail(CodeStatus, errors.New("AWS report has no deliveries"))
	}
	months := slices.CompactFunc(slices.Collect(func(yield func(time.Time) bool) {
		for _, delivery := range deliveries {
			if !yield(delivery.month) {
				return
			}
		}
	}), time.Time.Equal)
	return walkMonths(req, months, len(months), func(month time.Time) error {
		for _, delivery := range deliveries {
			if !delivery.month.Equal(month) {
				continue
			}
			if err := report.delivery(ctx, req, delivery, emit); !errors.Is(err, errMissing) {
				return err
			}
		}
		return fail(CodeStatus, errors.New("AWS delivery is incomplete"))
	})
}

func awsClient(ctx context.Context, req Cloud, scope awsScope, credential awsCredential) (*s3.Client, error) {
	resolved, err := s3.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, s3.EndpointParameters{Region: aws.String(scope.Region), Bucket: aws.String(scope.Bucket)})
	if err != nil {
		return nil, fail(CodeEndpoint, err)
	}
	endpoint := cmp.Or(req.S3Endpoint, resolved.URI.String())
	httpClient, err := req.Client(endpoint)
	if err != nil {
		return nil, fail(CodeEndpoint, err)
	}
	provider, err := credential.provider(ctx, req, scope.Region)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(aws.Config{Region: scope.Region, Credentials: provider, HTTPClient: httpClient, Retryer: func() aws.Retryer { return aws.NopRetryer{} }}, func(o *s3.Options) {
		if req.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(req.S3Endpoint)
			o.UsePathStyle = true
		}
	}), nil
}

func awsMonthPrefix(scope awsScope, month time.Time) string {
	return scope.ReportPrefix + "/" + month.Format("20060102") + "-" + month.AddDate(0, 1, 0).Format("20060102") + "/"
}

func (r awsReport) deliveries(ctx context.Context, months []time.Time) ([]awsDelivery, error) {
	deliveries := []awsDelivery{}
	seen := 0
	for _, month := range months {
		prefix := awsMonthPrefix(r.scope, month)
		paginator := s3.NewListObjectsV2Paginator(r.client, &s3.ListObjectsV2Input{Bucket: aws.String(r.scope.Bucket), Prefix: aws.String(prefix)})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, storageFailure(ctx, err)
			}
			seen += len(page.Contents)
			if seen > awsMaxFiles*4 {
				return nil, fail(CodeResponse, errors.New("too many AWS objects"))
			}
			for _, object := range page.Contents {
				key := aws.ToString(object.Key)
				pieces := strings.Split(strings.TrimPrefix(key, prefix), "/")
				if len(pieces) == 2 && strings.HasSuffix(pieces[1], "-Manifest.json") && object.LastModified != nil {
					deliveries = append(deliveries, awsDelivery{key: key, modified: *object.LastModified, month: month})
				}
			}
		}
	}
	slices.SortStableFunc(deliveries, func(a, b awsDelivery) int {
		return cmp.Or(b.month.Compare(a.month), b.modified.Compare(a.modified))
	})
	for index := 1; index < len(deliveries); index++ {
		previous, current := deliveries[index-1], deliveries[index]
		if previous.month.Equal(current.month) && previous.modified.Equal(current.modified) {
			return nil, fail(CodeResponse, errors.New("ambiguous AWS deliveries"))
		}
	}
	return deliveries, nil
}

func (r awsReport) delivery(ctx context.Context, req Cloud, delivery awsDelivery, emit func(Period) error) error {
	manifest, err := r.manifest(ctx, delivery)
	if err != nil {
		return err
	}
	start, err := time.Parse(awsManifestTime, manifest.BillingPeriod.Start)
	if err != nil {
		return fail(CodeResponse, err)
	}
	end, err := time.Parse(awsManifestTime, manifest.BillingPeriod.End)
	if err != nil || !end.After(start) {
		return fail(CodeResponse, errors.New("invalid AWS billing period"))
	}
	if !day(start).Equal(delivery.month) || !day(end).Equal(delivery.month.AddDate(0, 1, 0)) {
		return fail(CodeResponse, errors.New("AWS billing period does not match delivery"))
	}
	days := newDays()
	for t := day(start); t.Before(end) && t.Before(req.Today) && t.Before(day(delivery.modified)); t = t.AddDate(0, 0, 1) {
		if !t.Before(req.Earliest) {
			days.include(t)
		}
	}
	rows := awsRows{days: days, scope: r.scope, seen: map[[32]byte][32]byte{}}
	for _, part := range manifest.ReportKeys {
		if err := r.part(ctx, part, &rows); err != nil {
			return err
		}
	}
	return emit(Period{Days: days, Revision: &Revision{PeriodStart: day(start), ID: manifest.AssemblyID, OrderingAt: delivery.modified}})
}

func (r awsReport) manifest(ctx context.Context, delivery awsDelivery) (awsManifest, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.scope.Bucket), Key: aws.String(delivery.key)})
	if err != nil {
		return awsManifest{}, storageFailure(ctx, err)
	}
	defer out.Body.Close()
	remaining := min(int64(maxBodyBytes), awsMaxCompressed-r.budget.compressed, awsMaxExpanded-r.budget.expanded)
	if remaining < 0 {
		return awsManifest{}, fail(CodeResponse, errBudget)
	}
	data, err := io.ReadAll(io.LimitReader(out.Body, remaining+1))
	r.budget.compressed += int64(len(data))
	r.budget.expanded += int64(len(data))
	if int64(len(data)) > remaining && remaining < maxBodyBytes {
		return awsManifest{}, fail(CodeResponse, errBudget)
	}
	var manifest awsManifest
	if err != nil || int64(len(data)) > remaining || json.Unmarshal(data, &manifest) != nil || manifest.AssemblyID == "" || len(manifest.AssemblyID) > 256 || len(manifest.ReportKeys) == 0 || len(manifest.ReportKeys) > awsMaxFiles {
		return awsManifest{}, fail(CodeResponse, errors.New("invalid AWS manifest"))
	}
	dir := path.Dir(delivery.key) + "/"
	if path.Base(path.Dir(delivery.key)) != manifest.AssemblyID {
		return awsManifest{}, fail(CodeResponse, errors.New("AWS assembly does not match manifest"))
	}
	if slices.ContainsFunc(manifest.ReportKeys, func(key string) bool {
		return !strings.HasPrefix(key, dir) || strings.Contains(strings.TrimPrefix(key, dir), "/") || !strings.HasSuffix(key, ".csv.gz") && !strings.HasSuffix(key, ".csv.zip")
	}) {
		return awsManifest{}, fail(CodeResponse, errors.New("AWS delivery must use compressed CSV"))
	}
	return manifest, nil
}

type awsRangeReader struct {
	ctx      context.Context
	report   awsReport
	key      string
	requests int
}

func (r *awsRangeReader) ReadAt(p []byte, offset int64) (int, error) {
	r.requests++
	if r.requests > 1000 {
		return 0, errors.New("AWS ZIP exceeds request cap")
	}
	if int64(len(p)) > awsMaxCompressed-r.report.budget.compressed {
		return 0, errBudget
	}
	out, err := r.report.client.GetObject(r.ctx, &s3.GetObjectInput{Bucket: aws.String(r.report.scope.Bucket), Key: aws.String(r.key), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1))})
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	n, err := io.ReadFull(out.Body, p)
	r.report.budget.compressed += int64(n)
	return n, err
}

func (r awsReport) part(ctx context.Context, key string, rows *awsRows) error {
	if strings.HasSuffix(key, ".csv.zip") {
		return r.zipPart(ctx, key, rows)
	}
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.scope.Bucket), Key: aws.String(key)})
	if err != nil {
		return storageFailure(ctx, err)
	}
	defer out.Body.Close()
	if out.ContentLength != nil && *out.ContentLength > awsMaxCompressed {
		return fail(CodeResponse, errors.New("AWS part exceeds size cap"))
	}
	if out.ContentLength != nil && *out.ContentLength > awsMaxCompressed-r.budget.compressed {
		return fail(CodeResponse, errBudget)
	}
	remaining := awsMaxCompressed - r.budget.compressed
	bounded := &io.LimitedReader{R: out.Body, N: remaining + 1}
	defer func() { r.budget.compressed += remaining + 1 - bounded.N }()
	err = r.gzipPart(bounded, rows)
	if bounded.N == 0 {
		return fail(CodeResponse, errBudget)
	}
	return err
}

func (r awsReport) gzipPart(input io.Reader, rows *awsRows) error {
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fail(CodeResponse, err)
	}
	defer gz.Close()
	return r.csv(gz, rows)
}

func (r awsReport) zipPart(ctx context.Context, key string, rows *awsRows) error {
	head, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.scope.Bucket), Key: aws.String(key)})
	if err != nil {
		return storageFailure(ctx, err)
	}
	switch {
	case head.ContentLength == nil || *head.ContentLength <= 0:
		return fail(CodeResponse, errors.New("invalid AWS ZIP size"))
	case *head.ContentLength > awsMaxCompressed:
		return fail(CodeResponse, errors.New("AWS ZIP exceeds size cap"))
	case *head.ContentLength > awsMaxCompressed-r.budget.compressed:
		return fail(CodeResponse, errBudget)
	}
	archive, err := zip.NewReader(&awsRangeReader{ctx: ctx, report: r, key: key}, *head.ContentLength)
	if errors.Is(err, errBudget) {
		return fail(CodeResponse, errBudget)
	}
	if err != nil || len(archive.File) != 1 || !strings.HasSuffix(archive.File[0].Name, ".csv") {
		return fail(CodeResponse, errors.New("invalid AWS ZIP"))
	}
	if archive.File[0].UncompressedSize64 > awsMaxExpanded {
		return fail(CodeResponse, errors.New("AWS ZIP exceeds expanded size cap"))
	}
	if archive.File[0].UncompressedSize64 > uint64(awsMaxExpanded-r.budget.expanded) {
		return fail(CodeResponse, errBudget)
	}
	file, err := archive.File[0].Open()
	if err != nil {
		return fail(CodeResponse, err)
	}
	defer file.Close()
	return r.csv(file, rows)
}

type awsRows struct {
	days  Days
	scope awsScope
	count int
	keys  int
	seen  map[[32]byte][32]byte
}

func (r awsReport) csv(input io.Reader, rows *awsRows) error {
	remaining := awsMaxExpanded - r.budget.expanded
	bounded := &io.LimitedReader{R: input, N: remaining + 1}
	defer func() { r.budget.expanded += remaining + 1 - bounded.N }()
	err := rows.read(csv.NewReader(bounded))
	if bounded.N == 0 {
		return fail(CodeResponse, errBudget)
	}
	return err
}

func (rows *awsRows) read(reader *csv.Reader) error {
	header, err := reader.Read()
	if err != nil {
		return fail(CodeResponse, err)
	}
	columns := make(map[string]int, len(header))
	for n, name := range header {
		columns[name] = n
	}
	if slices.ContainsFunc(awsRequiredColumns, func(name string) bool { _, ok := columns[name]; return !ok }) {
		return fail(CodeResponse, errors.New("AWS CSV is missing a required column"))
	}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fail(CodeResponse, err)
		}
		if err := rows.add(columns, record); err != nil {
			return err
		}
	}
}

func (rows *awsRows) add(columns map[string]int, record []string) error {
	rows.count++
	if rows.count > awsMaxRows {
		return fail(CodeResponse, errors.New("AWS report exceeds row cap"))
	}
	field := func(name string) string {
		if n, ok := columns[name]; ok {
			return record[n]
		}
		return ""
	}
	if field("bill/PayerAccountId") != rows.scope.PayerAccountID {
		return fail(CodeResponse, errors.New("AWS payer account does not match scope"))
	}
	service, kind := field("lineItem/ProductCode"), field("lineItem/LineItemType")
	if len(rows.scope.LinkedAccountIDs) > 0 && !slices.Contains(rows.scope.LinkedAccountIDs, field("lineItem/UsageAccountId")) || service != "AmazonBedrock" && service != "AmazonBedrockService" {
		return nil
	}
	if duplicate, err := rows.duplicate(field, record); duplicate || err != nil {
		return err
	}
	if slices.Contains(awsSkippedLineTypes, kind) {
		return nil
	}
	stamp, err := time.Parse(time.RFC3339, field("lineItem/UsageStartDate"))
	if err != nil {
		return fail(CodeResponse, err)
	}
	if !rows.days.has(stamp) {
		return nil
	}
	amountField := map[string]string{"DiscountedUsage": "reservation/EffectiveCost", "SavingsPlanCoveredUsage": "savingsPlan/SavingsPlanEffectiveCost"}[kind]
	if _, ok := columns[cmp.Or(amountField, "lineItem/UnblendedCost")]; !ok {
		return fail(CodeResponse, errors.New("missing AWS effective cost"))
	}
	amount, err := decimal.NewFromString(field(cmp.Or(amountField, "lineItem/UnblendedCost")))
	if err != nil {
		return fail(CodeResponse, err)
	}
	before := rows.days.size(stamp)
	if err := rows.days.add(stamp, strings.Join([]string{service, kind, field("lineItem/UsageType"), field("lineItem/Operation"), field("product/sku")}, " | "), field("lineItem/CurrencyCode"), amount); err != nil {
		return err
	}
	rows.keys += rows.days.size(stamp) - before
	if rows.keys > awsMaxKeys {
		return fail(CodeResponse, errors.New("AWS report exceeds aggregation cap"))
	}
	return nil
}

func (rows *awsRows) duplicate(field func(string) string, record []string) (bool, error) {
	lineID := field("identity/LineItemId")
	if lineID == "" {
		return false, nil
	}
	id := sha256.Sum256([]byte(strings.Join([]string{lineID, cmp.Or(field("identity/TimeInterval"), field("lineItem/UsageStartDate")), field("bill/BillType")}, "\x00")))
	content := sha256.Sum256([]byte(strings.Join(record, "\x00")))
	previous, seen := rows.seen[id]
	if seen && previous != content {
		return true, fail(CodeResponse, errors.New("conflicting AWS line identity"))
	}
	rows.seen[id] = content
	return seen, nil
}
