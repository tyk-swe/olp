package vendor

import "slices"

type References struct{ Credential, Network string }

func (c Connection) References() References {
	if c.Network == nil {
		return References{Credential: c.CredentialRef}
	}
	return References{Credential: c.CredentialRef, Network: c.Network.CredentialID}
}

func (r References) IDs() []string {
	return slices.DeleteFunc([]string{r.Credential, r.Network}, func(id string) bool { return id == "" })
}
