package billing

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/connectors"
)

const (
	awsMaxFiles      = 1000
	awsMaxRows       = 2000000
	awsMaxKeys       = 100000
	awsMaxCompressed = 64 << 20
	awsMaxExpanded   = 512 << 20
)

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
	period   string
}

func (i *Importer) importAWS(ctx context.Context, row *importRow, cfg *providerConfig, key []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var scope awsScope
	var credential awsCredential
	if json.Unmarshal(row.Scope, &scope) != nil || json.Unmarshal(key, &credential) != nil {
		return failFetch("response", errors.New("invalid AWS import configuration"))
	}
	endpoint, err := s3.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, s3.EndpointParameters{Region: aws.String(scope.Region), Bucket: aws.String(scope.Bucket)})
	if err != nil {
		return failFetch("endpoint", err)
	}
	awsConfig := *cfg
	awsEndpoint := endpoint.URI.String()
	awsConfig.Endpoint = &awsEndpoint
	httpClient, err := i.client(ctx, row.ProviderID, &awsConfig, key)
	if err != nil {
		return failFetch("endpoint", err)
	}
	provider := aws.CredentialsProvider(credentials.NewStaticCredentialsProvider(credential.AccessKeyID, credential.SecretAccessKey, credential.SessionToken))
	if credential.AuthMode == "workload" {
		value, e := connectors.NewAuth(i.Egress).AWSWorkloadCredentials(ctx, scope.Region)
		if e != nil {
			return failFetch("credential", e)
		}
		provider = credentials.NewStaticCredentialsProvider(value.AccessKeyID, value.SecretAccessKey, value.SessionToken)
	}
	config := aws.Config{Region: scope.Region, Credentials: provider, HTTPClient: httpClient, Retryer: func() aws.Retryer { return aws.NopRetryer{} }}
	client := s3.NewFromConfig(config)
	if i.AWSClient != nil {
		client = i.AWSClient(config)
	}
	today := i.now().UTC().Truncate(24 * time.Hour)
	var retention int
	if err := i.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT value::int FROM olp.settings WHERE key='retention.usage_days'),90)`).Scan(&retention); err != nil {
		return err
	}
	earliest := today.AddDate(0, 0, -retention)
	if row.ImportedThrough != nil {
		earliest = time.Date(today.Year(), today.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	}
	deliveries, err := awsDeliveries(ctx, client, scope, earliest, today)
	if err != nil {
		return err
	}
	if len(deliveries) == 0 {
		return failFetch("status", errors.New("AWS report has no deliveries"))
	}
	period := ""
	complete := false
	for _, delivery := range deliveries {
		if delivery.period != period {
			if period != "" && !complete {
				return failFetch("status", errAWSIncomplete)
			}
			period, complete = delivery.period, false
		}
		if complete {
			continue
		}
		err := i.importAWSDelivery(ctx, row, client, scope, delivery, earliest, today)
		if errors.Is(err, errAWSIncomplete) {
			continue
		}
		if err != nil {
			return err
		}
		complete = true
	}
	if !complete {
		return failFetch("status", errAWSIncomplete)
	}
	return nil
}

func (i *Importer) importAWSDelivery(ctx context.Context, row *importRow, client *s3.Client, scope awsScope, delivery awsDelivery, earliest, today time.Time) error {
	manifest, err := awsReadManifest(ctx, client, scope, delivery)
	if err != nil {
		return err
	}
	start, err := time.Parse(time.RFC3339, manifest.BillingPeriod.Start)
	if err != nil {
		return failFetch("response", err)
	}
	end, err := time.Parse(time.RFC3339, manifest.BillingPeriod.End)
	if err != nil || !end.After(start) {
		return failFetch("response", errors.New("invalid AWS billing period"))
	}
	if start.UTC().Format("20060102")+"-"+end.UTC().Format("20060102") != delivery.period {
		return failFetch("response", errors.New("AWS billing period does not match delivery"))
	}
	days := reportAccumulator{}
	for day := start.UTC().Truncate(24 * time.Hour); day.Before(end) && day.Before(today); day = day.AddDate(0, 0, 1) {
		if !day.Before(earliest) {
			days.ensureDay(day.Format(dayFormat))
		}
	}
	state := awsReadState{seen: map[[32]byte][32]byte{}}
	for _, part := range manifest.ReportKeys {
		if err := awsReadPart(ctx, client, scope, part, days, &state); err != nil {
			return err
		}
	}
	if err := i.storeDays(ctx, row, start.UTC().Format(dayFormat), &sourceRevision{id: manifest.AssemblyID, orderingAt: delivery.modified}, days); err != nil {
		return err
	}
	return nil
}

var awsPeriodPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{8}$`)

func awsDeliveries(ctx context.Context, client *s3.Client, scope awsScope, earliest, today time.Time) ([]awsDelivery, error) {
	prefix := scope.ReportPrefix + "/"
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(scope.Bucket), Prefix: aws.String(prefix)})
	deliveries := []awsDelivery{}
	seen := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, awsError(err)
		}
		for _, object := range page.Contents {
			seen++
			if seen > awsMaxFiles*4 {
				return nil, failFetch("response", errors.New("too many AWS objects"))
			}
			key := aws.ToString(object.Key)
			rest := strings.TrimPrefix(key, prefix)
			pieces := strings.Split(rest, "/")
			if len(pieces) != 3 || !awsPeriodPattern.MatchString(pieces[0]) || !strings.HasSuffix(pieces[2], "-Manifest.json") || object.LastModified == nil {
				continue
			}
			start, err := time.Parse("20060102", pieces[0][:8])
			if err != nil || start.After(today) || start.AddDate(0, 1, 0).Before(earliest) {
				continue
			}
			deliveries = append(deliveries, awsDelivery{key: key, modified: *object.LastModified, period: pieces[0]})
		}
	}
	slices.SortFunc(deliveries, func(a, b awsDelivery) int {
		if order := strings.Compare(a.period, b.period); order != 0 {
			return order
		}
		return b.modified.Compare(a.modified)
	})
	for index := 1; index < len(deliveries); index++ {
		previous, current := deliveries[index-1], deliveries[index]
		if previous.period == current.period && previous.modified.Equal(current.modified) && previous.key != current.key {
			return nil, failFetch("response", errors.New("ambiguous AWS deliveries"))
		}
	}
	return deliveries, nil
}

func awsReadManifest(ctx context.Context, client *s3.Client, scope awsScope, delivery awsDelivery) (awsManifest, error) {
	var manifest awsManifest
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(scope.Bucket), Key: aws.String(delivery.key)})
	if err != nil {
		return manifest, awsError(err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes || json.Unmarshal(data, &manifest) != nil {
		return manifest, failFetch("response", errors.New("invalid AWS manifest"))
	}
	if manifest.AssemblyID == "" || len(manifest.AssemblyID) > 256 || len(manifest.ReportKeys) == 0 || len(manifest.ReportKeys) > awsMaxFiles {
		return manifest, failFetch("response", errors.New("invalid AWS manifest"))
	}
	dir := path.Dir(delivery.key) + "/"
	if path.Base(strings.TrimSuffix(dir, "/")) != manifest.AssemblyID {
		return manifest, failFetch("response", errors.New("AWS assembly does not match manifest"))
	}
	for _, key := range manifest.ReportKeys {
		if !strings.HasPrefix(key, dir) || strings.Contains(strings.TrimPrefix(key, dir), "/") || !(strings.HasSuffix(key, ".csv.gz") || strings.HasSuffix(key, ".csv.zip")) {
			return manifest, failFetch("response", errors.New("AWS delivery must use compressed CSV"))
		}
	}
	return manifest, nil
}

var errAWSIncomplete = errors.New("AWS delivery is incomplete")

func awsError(err error) error {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		if status.HTTPStatusCode() == 404 {
			return failFetch("status", errAWSIncomplete)
		}
		if status.HTTPStatusCode() == 401 || status.HTTPStatusCode() == 403 {
			return failFetch("credential", err)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failFetch("timeout", err)
	}
	return failFetch("network", err)
}

type awsReadState struct {
	rows int
	keys int
	seen map[[32]byte][32]byte
}

type awsRangeReader struct {
	ctx      context.Context
	client   *s3.Client
	bucket   string
	key      string
	requests int
}

func (r *awsRangeReader) ReadAt(p []byte, offset int64) (int, error) {
	r.requests++
	if r.requests > 1000 {
		return 0, errors.New("AWS ZIP exceeds request cap")
	}
	out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(r.key), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1))})
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	return io.ReadFull(out.Body, p)
}

func awsReadPart(ctx context.Context, client *s3.Client, scope awsScope, key string, days reportAccumulator, state *awsReadState) error {
	if strings.HasSuffix(key, ".csv.zip") {
		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(scope.Bucket), Key: aws.String(key)})
		if err != nil {
			return awsError(err)
		}
		if head.ContentLength == nil || *head.ContentLength <= 0 || *head.ContentLength > awsMaxCompressed {
			return failFetch("response", errors.New("invalid AWS ZIP size"))
		}
		reader := &awsRangeReader{ctx: ctx, client: client, bucket: scope.Bucket, key: key}
		archive, err := zip.NewReader(reader, *head.ContentLength)
		if err != nil || len(archive.File) != 1 || !strings.HasSuffix(archive.File[0].Name, ".csv") || archive.File[0].UncompressedSize64 > awsMaxExpanded {
			return failFetch("response", errors.New("invalid AWS ZIP"))
		}
		csvFile, err := archive.File[0].Open()
		if err != nil {
			return failFetch("response", err)
		}
		defer csvFile.Close()
		return awsReadCSV(csvFile, scope, days, state)
	}
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(scope.Bucket), Key: aws.String(key)})
	if err != nil {
		return awsError(err)
	}
	defer out.Body.Close()
	if out.ContentLength != nil && *out.ContentLength > awsMaxCompressed {
		return failFetch("response", errors.New("AWS part exceeds size cap"))
	}
	gz, err := gzip.NewReader(io.LimitReader(out.Body, awsMaxCompressed+1))
	if err != nil {
		return failFetch("response", err)
	}
	defer gz.Close()
	return awsReadCSV(gz, scope, days, state)
}

func awsReadCSV(input io.Reader, scope awsScope, days reportAccumulator, state *awsReadState) error {
	bounded := &io.LimitedReader{R: input, N: awsMaxExpanded + 1}
	reader := csv.NewReader(bounded)
	header, err := reader.Read()
	if err != nil {
		return failFetch("response", err)
	}
	columns := map[string]int{}
	for n, name := range header {
		columns[name] = n
	}
	required := []string{"bill/PayerAccountId", "lineItem/UsageAccountId", "lineItem/ProductCode", "lineItem/UsageStartDate", "lineItem/LineItemType", "lineItem/UsageType", "lineItem/Operation", "product/sku", "lineItem/CurrencyCode", "lineItem/UnblendedCost"}
	for _, name := range required {
		if _, ok := columns[name]; !ok {
			return failFetch("response", errors.New("AWS CSV is missing a required column"))
		}
	}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failFetch("response", err)
		}
		state.rows++
		if state.rows > awsMaxRows {
			return failFetch("response", errors.New("AWS report exceeds row cap"))
		}
		field := func(name string) string { return record[columns[name]] }
		if field("bill/PayerAccountId") != scope.PayerAccountID {
			return failFetch("response", errors.New("AWS payer account does not match scope"))
		}
		if len(scope.LinkedAccountIDs) > 0 && !slices.Contains(scope.LinkedAccountIDs, field("lineItem/UsageAccountId")) {
			continue
		}
		service := field("lineItem/ProductCode")
		if service != "AmazonBedrock" && service != "AmazonBedrockService" {
			continue
		}
		if idColumn, ok := columns["identity/LineItemId"]; ok && record[idColumn] != "" {
			id := sha256.Sum256([]byte(record[idColumn]))
			content := sha256.Sum256([]byte(strings.Join(record, "\x00")))
			if previous, exists := state.seen[id]; exists {
				if previous != content {
					return failFetch("response", errors.New("conflicting AWS line identity"))
				}
				continue
			}
			state.seen[id] = content
		}
		kind := field("lineItem/LineItemType")
		if slices.Contains([]string{"Tax", "SavingsPlanNegation", "SavingsPlanRecurringFee", "SavingsPlanUpfrontFee", "RIFee", "Fee"}, kind) {
			continue
		}
		stamp, err := time.Parse(time.RFC3339, field("lineItem/UsageStartDate"))
		if err != nil {
			return failFetch("response", err)
		}
		day := stamp.UTC().Format(dayFormat)
		if _, ok := days[day]; !ok {
			continue
		}
		currency := field("lineItem/CurrencyCode")
		if len(currency) != 3 || currency[0] < 'A' || currency[0] > 'Z' || currency[1] < 'A' || currency[1] > 'Z' || currency[2] < 'A' || currency[2] > 'Z' {
			return failFetch("response", errors.New("invalid AWS currency"))
		}
		amountField := "lineItem/UnblendedCost"
		if kind == "DiscountedUsage" {
			amountField = "reservation/EffectiveCost"
		}
		if kind == "SavingsPlanCoveredUsage" {
			amountField = "savingsPlan/SavingsPlanEffectiveCost"
		}
		if _, ok := columns[amountField]; !ok {
			return failFetch("response", errors.New("missing AWS effective cost"))
		}
		amount, err := decimal.NewFromString(field(amountField))
		if err != nil {
			return failFetch("response", err)
		}
		label := strings.Join([]string{service, kind, field("lineItem/UsageType"), field("lineItem/Operation"), field("product/sku")}, " | ")
		if len([]rune(label)) > 256 {
			digest := sha256.Sum256([]byte(label))
			label = string([]rune(label)[:239]) + "-" + hex.EncodeToString(digest[:8])
		}
		before := len(days[day])
		days.add(day, amount, currency, &label)
		if len(days[day]) > before {
			state.keys++
		}
		if state.keys > awsMaxKeys {
			return failFetch("response", errors.New("AWS report exceeds aggregation cap"))
		}
	}
	if bounded.N == 0 {
		return failFetch("response", errors.New("AWS part exceeds expanded size cap"))
	}
	return nil
}

func (i *Importer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}
