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
	batchStorageBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	batchStorageRole   = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`)
)

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
	if !batchStorageBucket.MatchString(bucket) || strings.Contains(bucket, "..") || net.ParseIP(bucket) != nil {
		return "", "", "", false
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

// ContainsPrefix also permits a bucket root as an output destination.
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

func VertexBatchJobsURL(project, location, jobID, action string) (string, error) {
	if project == "" || !cloudIdentifier.MatchString(project) || location == "" || !cloudIdentifier.MatchString(location) {
		return "", errors.New("Vertex batch jobs require a project and location")
	}
	base, err := url.Parse(DefaultEndpoint("vertex_ai", location, project))
	if err != nil {
		return "", err
	}
	root := strings.TrimSuffix(base.Path, "/publishers/google")
	if root == base.Path {
		return "", errors.New("Vertex endpoint does not have a publisher suffix")
	}
	path := root + "/batchPredictionJobs"
	if jobID != "" {
		path += "/" + url.PathEscape(jobID)
	}
	if action == "cancel" {
		if jobID == "" {
			return "", errors.New("Vertex batch cancellation requires a job ID")
		}
		path += ":cancel"
	} else if action != "" {
		return "", errors.New("unknown Vertex batch action")
	}
	if err := setEscapedPath(base, path); err != nil {
		return "", err
	}
	return base.String(), nil
}

func (c Config) VertexBatchJobsURL(jobID, action string) (string, error) {
	if endpoint, err := url.Parse(c.Endpoint); err == nil && cloudTestDestination(endpoint) {
		root := strings.TrimSuffix(endpoint.Path, "/publishers/google")
		if root == endpoint.Path {
			return "", errors.New("Vertex endpoint does not have a publisher suffix")
		}
		path := root + "/batchPredictionJobs"
		if jobID != "" {
			path += "/" + url.PathEscape(jobID)
		}
		if action == "cancel" {
			if jobID == "" {
				return "", errors.New("Vertex cancellation requires a job ID")
			}
			path += ":cancel"
		} else if action != "" {
			return "", errors.New("unknown Vertex batch action")
		}
		if err := setEscapedPath(endpoint, path); err != nil {
			return "", err
		}
		return endpoint.String(), nil
	}
	return VertexBatchJobsURL(c.CloudProject, c.CloudRegion, jobID, action)
}

func BedrockBatchJobsURL(region, jobID, action string) (string, error) {
	base, err := BedrockEndpoint(region, true)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Bedrock control-plane endpoint is invalid")
	}
	path := "/model-invocation-job"
	if jobID == "" {
		switch action {
		case "":
			path = "/model-invocation-jobs"
		case "create":
			path = "/model-invocation-job"
		default:
			return "", errors.New("Bedrock batch action requires a job ID")
		}
	} else {
		path += "/" + url.PathEscape(jobID)
		switch action {
		case "":
		case "stop":
			path += "/stop"
		default:
			return "", errors.New("unknown Bedrock batch action")
		}
	}
	if err := setEscapedPath(parsed, path); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func (c Config) BedrockBatchJobsURL(jobID, action string) (string, error) {
	if endpoint, err := url.Parse(c.Endpoint); err == nil && cloudTestDestination(endpoint) {
		path := "/model-invocation-job"
		if jobID == "" {
			if action == "" {
				path = "/model-invocation-jobs"
			} else if action != "create" {
				return "", errors.New("Bedrock batch action requires a job ID")
			}
		} else {
			path += "/" + url.PathEscape(jobID)
			if action == "stop" {
				path += "/stop"
			} else if action != "" {
				return "", errors.New("unknown Bedrock batch action")
			}
		}
		if err := setEscapedPath(endpoint, path); err != nil {
			return "", err
		}
		return endpoint.String(), nil
	}
	return BedrockBatchJobsURL(c.CloudRegion, jobID, action)
}

func setEscapedPath(u *url.URL, escaped string) error {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return err
	}
	u.Path, u.RawPath = decoded, escaped
	return nil
}
