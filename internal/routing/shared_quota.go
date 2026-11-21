package routing

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const maxSharedTokensPerMinute = int64(1)<<53 - 1

var sharedQuotaNamespace = uuid.MustParse("8f59d3c2-9c41-5d6e-9a2b-3f0e5a7c1d84")

type SharedTokenQuota struct {
	Model           string `json:"model"`
	Group           string `json:"group"`
	TokensPerMinute int64  `json:"tokens_per_minute"`
}

func ValidSharedQuotaGroup(group string) bool {
	if len(group) < 1 || len(group) > 64 {
		return false
	}
	if strings.Trim(group, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return false
	}
	return group[0] != '-' && group[len(group)-1] != '-'
}

func (q SharedTokenQuota) Validate(field string) error {
	if q.Model == "" {
		return invalidValue(field+".model", "Name the deployment model the shared quota covers.")
	}
	if !ValidSharedQuotaGroup(q.Group) {
		return invalidValue(field+".group", "Use 1 to 64 lowercase letters, digits or hyphens, starting and ending with a letter or digit.")
	}
	if q.TokensPerMinute < 1 || q.TokensPerMinute > maxSharedTokensPerMinute {
		return invalidValue(field+".tokens_per_minute", "Use a positive tokens per minute limit.")
	}
	return nil
}

func SharedTokenQuotaGroupID(projectID, group string) string {
	return uuid.NewSHA1(sharedQuotaNamespace, []byte(projectID+"\x00"+group)).String()
}

func (p *Provider) SharedTokenQuotaFor(model string) (SharedTokenQuota, string, bool) {
	if p == nil || model == "" || p.ProjectID == nil {
		return SharedTokenQuota{}, "", false
	}
	for _, quota := range p.SharedTokenQuotas {
		if quota.Model == model {
			return quota, SharedTokenQuotaGroupID(*p.ProjectID, quota.Group), true
		}
	}
	return SharedTokenQuota{}, "", false
}

func (s *Snapshot) validateSharedTokenQuotas() error {
	ceilings := map[string]int64{}
	for id, p := range s.Providers {
		if len(p.SharedTokenQuotas) == 0 {
			continue
		}
		if p.ProjectID == nil || *p.ProjectID == "" {
			return errSharedQuota(id, "assigns a shared token quota without a project")
		}
		seen := map[string]bool{}
		for _, quota := range p.SharedTokenQuotas {
			if err := quota.Validate("shared_token_quotas"); err != nil {
				return errSharedQuota(id, err.Error())
			}
			if seen[quota.Model] {
				return errSharedQuota(id, "assigns model "+quota.Model+" to more than one shared token quota")
			}
			seen[quota.Model] = true
			groupID := SharedTokenQuotaGroupID(*p.ProjectID, quota.Group)
			if ceiling, ok := ceilings[groupID]; ok && ceiling != quota.TokensPerMinute {
				return errSharedQuota(id, "declares a different tokens per minute ceiling for shared token quota group "+quota.Group)
			}
			ceilings[groupID] = quota.TokensPerMinute
		}
	}
	return nil
}

func errSharedQuota(providerID, problem string) error {
	return fmt.Errorf("provider %q %s", providerID, problem)
}
