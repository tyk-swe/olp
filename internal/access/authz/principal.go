package authz

import (
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
)

type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Active      bool      `json:"active"`
	AccessScope string    `json:"access_scope"`
	ETag        string    `json:"etag"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Principal is an authenticated management caller: a signed-in member
// ("user"), or a management token ("machine") acting within its creator's
// current authority.
type Principal struct {
	User
	Kind string
	// KeyAuthority is set only by an inference-key authentication alternative.
	// It grants consumer catalog reads, never management mutation authority.
	KeyAuthority     *keyauthority.Authority
	Creator          string
	AllProjects      bool
	Projects         map[string]string
	Organizations    map[string]string
	SessionID, Token string

	// scopes and creatorRole bound a management token's authority.
	scopes      operationSet
	creatorRole string
}

func (p Principal) UserID() string {
	if p.Kind == "machine" {
		return p.Creator
	}
	return p.ID
}
