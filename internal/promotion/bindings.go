package promotion

import "github.com/tyk-swe/olp/internal/secretstore"

type CredentialBinding struct {
	Secret    string
	Reference *secretstore.Reference
}

type BindingSet map[string]CredentialBinding
