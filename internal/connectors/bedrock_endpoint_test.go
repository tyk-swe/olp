package connectors

import "testing"

func TestBedrockEndpointUsesTheRegionalPartitionForInferenceAndDiscovery(t *testing.T) {
	for region, suffix := range map[string]string{
		"us-east-1":     "amazonaws.com",
		"cn-north-1":    "amazonaws.com.cn",
		"us-gov-west-1": "amazonaws.com",
		"us-iso-east-1": "c2s.ic.gov",
	} {
		for _, discovery := range []bool{false, true} {
			service := "bedrock-runtime"
			if discovery {
				service = "bedrock"
			}
			got, err := BedrockEndpoint(region, discovery)
			want := "https://" + service + "." + region + "." + suffix
			if err != nil || got != want {
				t.Fatalf("%s discovery=%v: got %q, %v; want %q", region, discovery, got, err, want)
			}
			if !discovery && DefaultEndpoint("bedrock", region, "") != want {
				t.Fatal("provider normalization bypassed partition resolution")
			}
		}
	}
}
