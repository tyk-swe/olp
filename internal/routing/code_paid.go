package routing

import (
	"reflect"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func CodePaidProtocol(provider Provider, model string) (code.Protocol, bool, bool) {
	connector := provider.Connector()
	if connector.CredentialSource == "caller" || len(provider.ParameterDefaults) > 0 || len(provider.OperationDefaults) > 0 || len(provider.Models) > 0 || provider.Plugin != nil {
		return "", false, false
	}
	if connector.Hosting() == "vertex-anthropic" || connector.Hosting() == "bedrock-anthropic" {
		return "", false, false
	}
	family, err := connector.TargetFamily(wire.Chat)
	if err != nil {
		return "", false, false
	}
	protocol, known := map[wire.Format]code.Protocol{
		wire.Responses: code.ProtocolResponses,
		wire.Anthropic: code.ProtocolMessages,
		wire.Chat:      code.ProtocolChat,
	}[family]
	if !known {
		return "", false, false
	}
	surface := family.Surface()
	streaming := provider.Supports(model, "generation", surface, "streaming")
	unary := provider.Supports(model, "generation", surface, "unary")
	if !streaming && !unary {
		return "", false, false
	}
	return protocol, streaming, true
}

func CodePaidLimitsMatch(liveProvider, frozenProvider *Limits, liveSlot, frozenSlot Slot) bool {
	return reflect.DeepEqual(liveProvider, frozenProvider) && reflect.DeepEqual(liveSlot.RequestsPerMinute, frozenSlot.RequestsPerMinute) && reflect.DeepEqual(liveSlot.TokensPerMinute, frozenSlot.TokensPerMinute) && reflect.DeepEqual(liveSlot.MaxConcurrency, frozenSlot.MaxConcurrency) && reflect.DeepEqual(liveSlot.Supply, frozenSlot.Supply)
}
