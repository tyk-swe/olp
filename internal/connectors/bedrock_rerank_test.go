package connectors

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestBedrockRerankAddressesTheAgentRuntime(t *testing.T) {
	c := Config{Kind: "bedrock", AuthMode: "default_chain", CloudRegion: "us-west-2", VendorID: "amazon-bedrock", Endpoint: DefaultEndpoint("bedrock", "us-west-2", "")}
	if got, err := c.URL(openai.FamilyBedrockRerank, "cohere.rerank-v3-5:0", false); err != nil || got != "https://bedrock-agent-runtime.us-west-2.amazonaws.com/rerank" {
		t.Fatalf("URL = %s, %v", got, err)
	}
	if !c.Supports("rerank", "openai", "unary") || c.awsService() != "bedrock" {
		t.Fatal("Bedrock rerank is not served or not signed for bedrock")
	}
	c.Endpoint = "https://vpce-0abc.bedrock-runtime.us-west-2.vpce.amazonaws.com"
	if _, err := c.URL(openai.FamilyBedrockRerank, "cohere.rerank-v3-5:0", false); err == nil {
		t.Fatal("a VPC endpoint addressed the public Agent Runtime")
	}
	for region, want := range map[string]string{
		"us-west-2":     "arn:aws:bedrock:us-west-2::foundation-model/amazon.rerank-v1:0",
		"cn-north-1":    "arn:aws-cn:bedrock:cn-north-1::foundation-model/amazon.rerank-v1:0",
		"us-gov-west-1": "arn:aws-us-gov:bedrock:us-gov-west-1::foundation-model/amazon.rerank-v1:0",
	} {
		if got := (Config{CloudRegion: region}).BedrockModelARN("amazon.rerank-v1:0"); got != want {
			t.Fatalf("ARN in %s = %s", region, got)
		}
	}
	if arn := "arn:aws:bedrock:us-west-2:123456789012:provisioned-model/abc"; (Config{CloudRegion: "us-west-2"}).BedrockModelARN(arn) != arn {
		t.Fatal("an ARN was rewritten")
	}
	if (Config{Kind: "openai_compatible", VendorID: "amazon-bedrock"}).Supports("rerank", "openai", "unary") {
		t.Fatal("another connector borrowed Bedrock's rerank contract")
	}
}
