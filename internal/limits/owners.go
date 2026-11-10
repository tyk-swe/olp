package limits

import (
	"encoding/hex"

	"github.com/google/uuid"
)

type OwnerKind string

const (
	OwnerKey       OwnerKind = "key"
	OwnerGroup     OwnerKind = "group"
	OwnerEndUser   OwnerKind = "end_user"
	OwnerAggregate OwnerKind = "aggregate"
	OwnerSupply    OwnerKind = "supply"
)

func (k OwnerKind) Valid() bool {
	switch k {
	case OwnerKey, OwnerGroup, OwnerEndUser, OwnerAggregate, OwnerSupply:
		return true
	default:
		return false
	}
}

type Owner struct {
	ID    string
	Kind  OwnerKind
	Basis Basis
}

var (
	aggregateNamespace = uuid.MustParse("e7d0305c-722f-5484-99a2-69a019fd74d5")
	endUserNamespace   = uuid.MustParse("15dc4671-a780-5723-8275-fdb6c9f90eb4")
)

func AggregateBudgetID(level, subject string) string {
	return uuid.NewSHA1(aggregateNamespace, []byte(level+"\x00"+subject)).String()
}

func KeyRouteBudgetID(key, route string) string {
	return AggregateBudgetID("key_route", key+"\x00"+route)
}

func AttributionBudgetID(project, key, value string) string {
	return AggregateBudgetID("attribution", project+"\x00"+key+"\x00"+value)
}

func EndUserKeyID(keyID, digest string) string {
	return uuid.NewSHA1(endUserNamespace, []byte("key\x00"+keyID+"\x00"+digest)).String()
}

func EndUserProjectID(projectID, digest string) string {
	return uuid.NewSHA1(endUserNamespace, []byte("project\x00"+projectID+"\x00"+digest)).String()
}

func SimpleUUID(value string) string {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return value
	}
	return hex.EncodeToString(parsed[:])
}

func CanonicalUUID(value string) string {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return value
	}
	return parsed.String()
}

func ConnectionLookup(providerID string) string { return "pc_" + SimpleUUID(providerID) }

func SlotLookup(slotID string) string { return "ps_" + SimpleUUID(slotID) }

func BudgetGroupLookup(id string) string { return "bg_" + SimpleUUID(id) }

func RouteLookup(routeID string) string { return "rt_" + SimpleUUID(routeID) }

func AggregateBudgetLookup(owner string) string { return "ab_" + SimpleUUID(owner) }

func EndUserLookup(accountID string) string { return "eu_" + SimpleUUID(accountID) }
