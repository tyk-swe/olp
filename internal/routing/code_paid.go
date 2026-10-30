package routing

import (
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"reflect"
)

func CodePaidProtocol(provider Provider, model string) (codemode.Protocol, bool) {
	connector := provider.Connector()
	if connector.CredentialSource == "caller" || len(provider.ParameterDefaults) > 0 || len(provider.OperationDefaults) > 0 || len(provider.Models) > 0 || provider.Plugin != nil {
		return "", false
	}
	if connector.Hosting() == "vertex-anthropic" || connector.Hosting() == "bedrock-anthropic" {
		return "", false
	}
	family, err := connector.TargetFamily(wire.Chat)
	if err != nil {
		return "", false
	}
	protocol, known := map[wire.Format]codemode.Protocol{wire.Responses: codemode.ProtocolResponses, wire.Anthropic: codemode.ProtocolMessages, wire.Chat: codemode.ProtocolChat}[family]
	if !known || !provider.Supports(model, "generation", family.Surface(), "unary") && !provider.Supports(model, "generation", family.Surface(), "streaming") {
		return "", false
	}
	return protocol, true
}

func CodePaidLimitsMatch(liveProvider, frozenProvider *Limits, liveSlot, frozenSlot Slot) bool {
	return reflect.DeepEqual(liveProvider, frozenProvider) && reflect.DeepEqual(liveSlot.RequestsPerMinute, frozenSlot.RequestsPerMinute) && reflect.DeepEqual(liveSlot.TokensPerMinute, frozenSlot.TokensPerMinute) && reflect.DeepEqual(liveSlot.MaxConcurrency, frozenSlot.MaxConcurrency) && reflect.DeepEqual(liveSlot.Supply, frozenSlot.Supply)
}
