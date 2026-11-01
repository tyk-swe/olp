package configuration

import (
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type codeMappedCandidate struct {
	ID             string
	Project        string
	ScopeMatches   bool
	NaturalMatches bool
}

func codeMappedCandidates(rows pgx.Rows, err error) ([]codeMappedCandidate, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[codeMappedCandidate])
}
func codeDestinationID(kind, project, ref string) (string, error) {
	reference, err := access.ParseUUID(ref)
	if err != nil {
		return "", err
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(codeIdentityKey(APIVersion, "code", kind, project, reference))).String(), nil
}
func codeMappedIdentity(project, ref, mapped string, unique bool, candidates []codeMappedCandidate) (string, bool, error) {
	reference, err := access.ParseUUID(ref)
	if err != nil {
		return "", false, err
	}
	direct := slices.IndexFunc(candidates, func(candidate codeMappedCandidate) bool {
		return candidate.ID == reference && candidate.Project == project
	})
	if direct >= 0 {
		if !candidates[direct].ScopeMatches {
			return "", false, access.Invalid("code", "A destination artifact reference cannot change its immutable resource scope.")
		}
		return reference, true, nil
	}
	stable := slices.IndexFunc(candidates, func(candidate codeMappedCandidate) bool { return candidate.ID == mapped })
	if stable >= 0 {
		if candidates[stable].Project != project || !candidates[stable].ScopeMatches {
			return "", false, access.Invalid("code", "A mapped destination resource cannot change its immutable scope.")
		}
		return mapped, true, nil
	}
	natural := func(candidate codeMappedCandidate) bool {
		return candidate.Project == project && candidate.NaturalMatches
	}
	first := slices.IndexFunc(candidates, natural)
	if first < 0 {
		return mapped, false, nil
	}
	if unique && !slices.ContainsFunc(candidates[first+1:], natural) {
		return candidates[first].ID, true, nil
	}
	return "", false, access.Invalid("code", "The destination has ambiguous code resource identities.")
}
