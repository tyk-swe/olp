package connectors

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

type BatchStorage struct {
	InputURIPrefix  string `json:"input_uri_prefix"`
	OutputURIPrefix string `json:"output_uri_prefix"`
	RoleARN         string `json:"role_arn,omitempty"`
}

var (
	s3BucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	gcsStorageBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)
	batchStorageRole = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`)
)

func ValidS3Bucket(bucket string) bool {
	return s3BucketPattern.MatchString(bucket) && !strings.Contains(bucket, "..") && net.ParseIP(bucket) == nil
}

func NormalizeBatchStorage(kind, profile string, storage *BatchStorage) (*BatchStorage, error) {
	vertex := kind == "vertex_ai" && profile == "vertex-batch-prediction"
	bedrock := kind == "bedrock" && profile == "bedrock-batch-inference"
	if !vertex && !bedrock {
		if storage != nil {
			return nil, errors.New("batch_storage applies only to the Vertex batch prediction and Bedrock batch inference profiles")
		}
		return nil, nil
	}
	if storage == nil {
		return nil, errors.New("this batch profile requires batch_storage")
	}
	out := *storage
	scheme := "gs"
	if vertex {
		if out.RoleARN != "" {
			return nil, errors.New("role_arn is only valid for Bedrock batch storage")
		}
	} else {
		scheme = "s3"
		if !batchStorageRole.MatchString(out.RoleARN) {
			return nil, errors.New("Bedrock batch storage requires a valid IAM role ARN")
		}
	}
	var err error
	out.InputURIPrefix, err = normalizeBatchStoragePrefix(out.InputURIPrefix, scheme)
	if err != nil {
		return nil, errors.New("input_uri_prefix: " + err.Error())
	}
	out.OutputURIPrefix, err = normalizeBatchStoragePrefix(out.OutputURIPrefix, scheme)
	if err != nil {
		return nil, errors.New("output_uri_prefix: " + err.Error())
	}
	return &out, nil
}

func normalizeBatchStoragePrefix(uri, scheme string) (string, error) {
	actualScheme, bucket, object, ok := parseBatchStorageURI(uri)
	if !ok || actualScheme != scheme {
		return "", errors.New("use a valid " + scheme + " URI prefix")
	}
	if len(object) > 0 && !strings.HasSuffix(object, "/") {
		object += "/"
	}
	normalized := scheme + "://" + bucket + "/" + object
	if len(normalized) > 1024 {
		return "", errors.New("URI prefixes must be at most 1024 bytes")
	}
	return normalized, nil
}

func parseBatchStorageURI(uri string) (scheme, bucket, object string, ok bool) {
	if len(uri) == 0 || len(uri) > 1024 || strings.ContainsAny(uri, `\%?#`) {
		return "", "", "", false
	}
	for _, r := range uri {
		if r < 0x20 || r == 0x7f {
			return "", "", "", false
		}
	}
	scheme, rest, found := strings.Cut(uri, "://")
	if !found || (scheme != "gs" && scheme != "s3") {
		return "", "", "", false
	}
	bucket, object, found = strings.Cut(rest, "/")
	if !found {
		object = ""
	}
	if scheme == "s3" && !ValidS3Bucket(bucket) {
		return "", "", "", false
	}
	if scheme == "gs" {
		if !gcsStorageBucket.MatchString(bucket) || strings.Contains(bucket, "..") || net.ParseIP(bucket) != nil {
			return "", "", "", false
		}
		for _, component := range strings.Split(bucket, ".") {
			if len(component) > 63 {
				return "", "", "", false
			}
		}
	}
	if object == "" {
		return scheme, bucket, "", true
	}
	trimmed := strings.TrimSuffix(object, "/")
	if trimmed == "" {
		return scheme, bucket, "", true
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", "", "", false
		}
	}
	return scheme, bucket, object, true
}

func (b BatchStorage) Contains(prefixURI, candidate string) bool {
	return b.contains(prefixURI, candidate, false)
}

func (b BatchStorage) ContainsPrefix(prefixURI, candidate string) bool {
	return b.contains(prefixURI, candidate, true)
}

func (b BatchStorage) contains(prefixURI, candidate string, allowRoot bool) bool {
	prefixScheme, prefixBucket, prefixObject, ok := parseBatchStorageURI(prefixURI)
	if !ok {
		return false
	}
	candidateScheme, candidateBucket, candidateObject, ok := parseBatchStorageURI(candidate)
	if !ok || candidateScheme != prefixScheme || candidateBucket != prefixBucket || candidateObject == "" && !allowRoot {
		return false
	}
	if prefixObject == "" {
		return true
	}
	prefixObject = strings.TrimSuffix(prefixObject, "/") + "/"
	return strings.HasPrefix(candidateObject, prefixObject)
}

func (c Config) VertexBatchJobsURL(jobID, action string) (string, error) {
	if !cloudIdentifier.MatchString(c.CloudProject) || !cloudIdentifier.MatchString(c.CloudRegion) {
		return "", errors.New("Vertex batch jobs require a project and location")
	}
	jobs := googleLocation(vertexAI, c.CloudRegion, c.CloudProject) + "/batchPredictionJobs"
	switch {
	case action != "" && action != "cancel":
		return "", errors.New("unknown Vertex batch action")
	case action == "cancel" && jobID == "":
		return "", errors.New("Vertex batch cancellation requires a job ID")
	case jobID == "":
		return jobs, nil
	case action == "cancel":
		return jobs + "/" + url.PathEscape(jobID) + ":cancel", nil
	}
	return jobs + "/" + url.PathEscape(jobID), nil
}

func (c Config) BedrockBatchJobsURL(jobID, action string) (string, error) {
	base, err := BedrockEndpoint(c.CloudRegion, true)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Bedrock control-plane endpoint is invalid")
	}
	path, err := bedrockBatchPath(jobID, action)
	if err != nil {
		return "", err
	}
	if err := setEscapedPath(parsed, path); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func bedrockBatchPath(jobID, action string) (string, error) {
	job := "/model-invocation-job/" + url.PathEscape(jobID)
	switch {
	case jobID == "" && action == "":
		return "/model-invocation-jobs", nil
	case jobID == "" && action == "create":
		return "/model-invocation-job", nil
	case jobID == "":
		return "", errors.New("Bedrock batch action requires a job ID")
	case action == "":
		return job, nil
	case action == "stop":
		return job + "/stop", nil
	}
	return "", errors.New("unknown Bedrock batch action")
}

func setEscapedPath(u *url.URL, escaped string) error {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return err
	}
	u.Path, u.RawPath = decoded, escaped
	return nil
}
