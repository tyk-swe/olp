package cloudbatches

import (
	"cmp"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type jobPlan struct {
	id         uuid.UUID
	endpoint   string
	body       []byte
	window     time.Duration
	submission batches.Submission
}

type vertexJob struct {
	DisplayName  string            `json:"displayName"`
	Model        string            `json:"model"`
	InputConfig  vertexInput       `json:"inputConfig"`
	OutputConfig vertexOutput      `json:"outputConfig"`
	Labels       map[string]string `json:"labels"`
}

type vertexInput struct {
	InstancesFormat string `json:"instancesFormat"`
	GCSSource       struct {
		URIs []string `json:"uris"`
	} `json:"gcsSource"`
}

type vertexOutput struct {
	PredictionsFormat string `json:"predictionsFormat"`
	GCSDestination    struct {
		OutputURIPrefix string `json:"outputUriPrefix"`
	} `json:"gcsDestination"`
}

type bedrockJob struct {
	JobName            string `json:"jobName"`
	RoleARN            string `json:"roleArn"`
	ModelID            string `json:"modelId"`
	ClientRequestToken string `json:"clientRequestToken"`
	TimeoutHours       int    `json:"timeoutDurationInHours,omitempty"`
	InputDataConfig    struct {
		S3InputDataConfig struct {
			S3URI    string `json:"s3Uri"`
			S3Format string `json:"s3InputFormat"`
		} `json:"s3InputDataConfig"`
	} `json:"inputDataConfig"`
	OutputDataConfig struct {
		S3OutputDataConfig struct {
			S3URI string `json:"s3Uri"`
		} `json:"s3OutputDataConfig"`
	} `json:"outputDataConfig"`
}

func planJob(p *kernel.Pin, adapter connectors.BatchAdapter, input request) (jobPlan, error) {
	id := uuid.Must(uuid.NewV7())
	label := strings.ReplaceAll(id.String(), "-", "")
	display := cmp.Or(input.DisplayName, "olp-"+label)
	window := kernel.DefaultBatchCompletionWindow
	if input.TimeoutHours > 0 {
		window = time.Duration(input.TimeoutHours) * time.Hour
	}
	cfg := p.Provider.Connector()
	if adapter == connectors.BatchVertex {
		endpoint, err := cfg.VertexBatchJobsURL("", "")
		job := vertexJob{
			DisplayName: display, Model: "publishers/google/models/" + cfg.Model(p.Model),
			InputConfig: vertexInput{InstancesFormat: "jsonl"}, OutputConfig: vertexOutput{PredictionsFormat: "jsonl"},
			Labels: map[string]string{"olp_submission": label},
		}
		job.InputConfig.GCSSource.URIs = []string{input.InputURI}
		job.OutputConfig.GCSDestination.OutputURIPrefix = input.OutputURIPrefix
		return jobPlan{id: id, endpoint: endpoint, body: kernel.MustJSON(job), window: window, submission: batches.VertexSubmission{Label: label}}, err
	}
	endpoint, err := cfg.BedrockBatchJobsURL("", "create")
	job := bedrockJob{
		JobName: display, RoleARN: p.Provider.BatchStorage.RoleARN, ModelID: cfg.Model(p.Model),
		ClientRequestToken: id.String(), TimeoutHours: input.TimeoutHours,
	}
	job.InputDataConfig.S3InputDataConfig.S3URI = input.InputURI
	job.InputDataConfig.S3InputDataConfig.S3Format = "JSONL"
	job.OutputDataConfig.S3OutputDataConfig.S3URI = input.OutputURIPrefix
	body := kernel.MustJSON(job)
	return jobPlan{
		id: id, endpoint: endpoint, body: body, window: window,
		submission: batches.BedrockSubmission{ClientRequestToken: id.String(), CreateBody: body},
	}, err
}

func acceptedJob(adapter connectors.BatchAdapter, result []byte, now time.Time) ([]byte, error) {
	if adapter != connectors.BatchBedrock {
		return result, nil
	}
	var created struct {
		ARN string `json:"jobArn"`
	}
	if err := json.Unmarshal(result, &created); err != nil || created.ARN == "" {
		return nil, errors.New("bedrock create response has no job ARN")
	}
	return json.Marshal(struct {
		ARN        string    `json:"jobArn"`
		Status     string    `json:"status"`
		SubmitTime time.Time `json:"submitTime"`
	}{ARN: created.ARN, Status: "Submitted", SubmitTime: now.UTC()})
}

func cancelAction(adapter connectors.BatchAdapter) string {
	if adapter == connectors.BatchBedrock {
		return "stop"
	}
	return "cancel"
}

func controlURL(cfg connectors.Config, adapter connectors.BatchAdapter, upstreamID, action string) (string, error) {
	if adapter == connectors.BatchVertex {
		return cfg.VertexBatchJobsURL(upstreamID, action)
	}
	return cfg.BedrockBatchJobsURL(upstreamID, action)
}
