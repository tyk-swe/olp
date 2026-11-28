package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/crewjam/saml"
	"github.com/jackc/pgx/v5"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/identity/samlidentity"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

// SAMLDefinition is portable public trust and role-mapping policy. Installation
// identity, SP certificates/private keys, and enrolled identities are excluded.
type SAMLDefinition struct {
	Enabled         bool          `json:"enabled"`
	Metadata        string        `json:"metadata_xml"`
	EmailAttribute  string        `json:"email_attribute"`
	GroupsAttribute string        `json:"groups_attribute"`
	NameAttribute   string        `json:"name_attribute"`
	DefaultRole     *string       `json:"default_role"`
	EmailMappings   []roleMapping `json:"email_role_mappings"`
	GroupMappings   []roleMapping `json:"group_role_mappings"`
}

type samlConfiguration struct {
	SAMLDefinition
	ID                string `json:"id"`
	ETag              string `json:"etag"`
	EntityID          string `json:"idp_entity_id"`
	ServiceProviderID string `json:"service_provider_id"`
	Certificate       string `json:"signing_certificate"`
}

type samlImportResponse struct {
	MetadataXML string `json:"metadata_xml"`
	IDPEntityID string `json:"idp_entity_id"`
}

func ValidateSAMLDefinition(d SAMLDefinition) error {
	_, err := validateSAML(samlConfiguration{SAMLDefinition: d})
	return err
}

type samlSigningKey struct {
	Private     []byte `json:"private"`
	Certificate []byte `json:"certificate"`
}

func ReadSAMLDefinition(ctx context.Context, q database.Queryer) (SAMLDefinition, bool, error) {
	var definition SAMLDefinition
	err := q.QueryRow(ctx, "SELECT document FROM olp.saml_configuration WHERE singleton").Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return SAMLDefinition{}, false, nil
	}
	return definition, err == nil, err
}

func loadSAML(ctx context.Context, q database.Queryer) (samlConfiguration, error) {
	var c samlConfiguration
	var id, etag string
	err := q.QueryRow(ctx, "SELECT document,id::text,etag::text FROM olp.saml_configuration WHERE singleton").Scan(&c, &id, &etag)
	if err != nil {
		return samlConfiguration{}, err
	}
	c.ID = id
	c.ETag = etag
	return c, nil
}

func (s *samlService) register(mux *http.ServeMux) {
	s.access.Public(mux, "POST /api/v1/saml/login", s.beginSAMLLogin)
	s.access.Public(mux, "POST /api/v1/saml/acs", s.samlACS, access.MaxBody(2<<20), access.AnyOrigin())
	s.access.Public(mux, "GET /api/v1/saml/complete", s.completeSAML, access.CrossSiteNavigation())
	s.access.Route(mux, "GET /api/v1/saml/configuration", s.samlConfiguration)
	s.access.Route(mux, "PUT /api/v1/saml/configuration", s.putSAMLConfiguration, access.MaxBody(2<<20))
	s.access.Route(mux, "POST /api/v1/saml/import", s.importSAMLMetadata)
	s.access.Stream(mux, "GET /api/v1/saml/metadata", s.samlMetadata)
	s.access.Route(mux, "GET /api/v1/profile/saml-identities", s.samlIdentities)
	s.access.Route(mux, "POST /api/v1/profile/saml/link", s.beginSAMLLink)
	s.access.Route(mux, "POST /api/v1/profile/saml/reauthenticate", s.beginSAMLReauthentication)
	s.access.Route(mux, "DELETE /api/v1/profile/saml-identities/{identity_id}", s.unlinkSAML)
}

func (s *samlService) samlConfiguration(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	c, err := loadSAML(r.Context(), s.access.Pool)
	return httpkit.Detail(c, c.ETag), err
}

func validateSAMLMetadataTrust(m samlidentity.Metadata, enabled bool, now time.Time) error {
	if enabled && !m.Entity.ValidUntil.IsZero() && !m.Entity.ValidUntil.After(now) {
		return problem.Invalid("metadata_xml", "The imported identity metadata has expired.")
	}
	if enabled && !slices.ContainsFunc(m.Certificates, func(cert *x509.Certificate) bool {
		return !now.Before(cert.NotBefore) && !now.After(cert.NotAfter)
	}) {
		return problem.Invalid("metadata_xml", "The identity metadata needs a currently valid signing certificate.")
	}
	if access.CheckIdentityURL(m.RedirectURL) != nil {
		return problem.Invalid("metadata_xml", "The identity provider sign-in endpoint must use a safe HTTPS URL.")
	}
	return nil
}

func validateSAMLAttributes(c samlConfiguration) error {
	attrs := []struct {
		field    string
		value    string
		required bool
	}{
		{"email_attribute", c.EmailAttribute, true},
		{"groups_attribute", c.GroupsAttribute, false},
		{"name_attribute", c.NameAttribute, false},
	}
	for _, attr := range attrs {
		if attr.value == "" && !attr.required {
			continue
		}
		if textfield.Validate(attr.field, attr.value, 256) != nil {
			return problem.Invalid(attr.field, "Use a bounded SAML attribute name.")
		}
	}
	return nil
}

func validateSAML(c samlConfiguration) (samlidentity.Metadata, error) {
	m, err := samlidentity.ParseMetadata([]byte(c.Metadata))
	if err != nil {
		return m, problem.Invalid("metadata_xml", "Import one SAML 2.0 IdP descriptor with signing certificates and an HTTP-Redirect sign-in endpoint.")
	}
	if err := validateSAMLMetadataTrust(m, c.Enabled, time.Now()); err != nil {
		return m, err
	}
	if err := validateSAMLAttributes(c); err != nil {
		return m, err
	}
	if c.DefaultRole != nil && !authz.ValidRole(*c.DefaultRole) {
		return m, problem.Invalid("default_role", "Choose a valid role.")
	}
	if err := validateRoleMappings(c.EmailMappings, c.GroupMappings, "email_role_mappings", "group_role_mappings", "Use unique values and valid roles."); err != nil {
		return m, err
	}
	return m, nil
}

func samlMappedRole(c samlConfiguration, address string, groups []string) string {
	return resolveMappedRole(roleMappingPolicy{DefaultRole: c.DefaultRole, EmailMappings: c.EmailMappings, GroupMappings: c.GroupMappings}, address, groups)
}

func newSAMLSigningKey() (samlSigningKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return samlSigningKey{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return samlSigningKey{}, err
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "OpenLLMProxy SAML signing"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return samlSigningKey{}, err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	return samlSigningKey{Private: priv, Certificate: der}, err
}

func (s *samlService) samlProvider(r *http.Request, q database.Queryer, c samlConfiguration) (*saml.ServiceProvider, samlidentity.Metadata, error) {
	m, err := validateSAML(c)
	if err != nil {
		return nil, m, err
	}
	raw, err := s.access.Keys.Read(r.Context(), q, s.access.Installation, c.ID, secrets.SAMLKey)
	if err != nil {
		return nil, m, err
	}
	defer clear(raw)
	var key samlSigningKey
	if err = json.Unmarshal(raw, &key); err != nil {
		return nil, m, err
	}
	defer clear(key.Private)
	k, err := x509.ParsePKCS8PrivateKey(key.Private)
	if err != nil {
		return nil, m, err
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, m, errors.New("invalid SAML signing key")
	}
	cert, err := x509.ParseCertificate(key.Certificate)
	if err != nil {
		return nil, m, err
	}
	metadataURL, err := url.Parse(s.access.Origin + "/api/v1/saml/metadata")
	if err != nil {
		return nil, m, err
	}
	acs, err := url.Parse(s.access.Origin + "/api/v1/saml/acs")
	if err != nil {
		return nil, m, err
	}
	sp := &saml.ServiceProvider{
		EntityID:          s.access.Origin + "/api/v1/saml/metadata",
		Key:               rsaKey,
		Certificate:       cert,
		MetadataURL:       *metadataURL,
		AcsURL:            *acs,
		IDPMetadata:       m.Entity,
		SignatureMethod:   dsig.RSASHA256SignatureMethod,
		AuthnNameIDFormat: saml.PersistentNameIDFormat,
		HTTPClient:        s.access.IdentityClient(),
	}
	return sp, m, nil
}

func (s *samlService) putSAMLConfiguration(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		SAMLDefinition
		Rotate bool `json:"rotate_signing_key"`
	}
	if err := httpkit.DecodeUnique(r, &input, 2<<20); err != nil {
		return httpkit.Reply{}, err
	}
	if err := ValidateSAMLDefinition(input.SAMLDefinition); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		old, err := loadSAML(r.Context(), tx)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return httpkit.Reply{}, access.Event{}, err
		}
		if old.ID != "" {
			if err = httpkit.Match(r, old.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		} else if r.Header.Get("If-Match") != "" {
			return httpkit.Reply{}, access.Event{}, problem.Fail(412, "etag_mismatch", "No SAML configuration exists.")
		}
		c, err := s.storeDefinition(r.Context(), tx, p, input.SAMLDefinition, input.Rotate)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(c, c.ETag), access.Event{Action: "saml.configuration.update", Resource: "saml_configuration", ID: c.ID}, nil
	})
}

func (s *Server) StoreSAMLDefinition(ctx context.Context, tx pgx.Tx, p authz.Principal, d SAMLDefinition, rotate bool) (samlConfiguration, error) {
	return s.saml.storeDefinition(ctx, tx, p, d, rotate)
}

func (s *samlService) rotateSAMLSigningKey(ctx context.Context, tx pgx.Tx, configID string) (string, error) {
	key, err := newSAMLSigningKey()
	if err != nil {
		return "", err
	}
	defer clear(key.Private)
	raw, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, configID, secrets.SAMLKey, raw, nil); err != nil {
		return "", err
	}
	return encodeSAMLCertificate(key.Certificate), nil
}

func (s *samlService) invalidateSAMLAfterConfigChange(ctx context.Context, tx pgx.Tx, old, next samlConfiguration) error {
	// Role evidence comes from the IdP's signed attributes, which a new SP
	// signing key leaves unchanged.
	if old.EntityID != next.EntityID || old.EmailAttribute != next.EmailAttribute || old.GroupsAttribute != next.GroupsAttribute {
		if _, err := tx.Exec(ctx, "UPDATE olp.saml_identities SET role_claims=NULL"); err != nil {
			return err
		}
	}
	if err := EnsureUsableOwner(ctx, tx, s.access.LocalLoginDisabled()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE purpose=$1", secrets.SAMLFlow); err != nil {
		return err
	}
	if !next.Enabled {
		if _, err := tx.Exec(ctx, "DELETE FROM olp.sessions WHERE auth_method='saml'"); err != nil {
			return err
		}
	}
	return nil
}

func (s *samlService) storeDefinition(ctx context.Context, tx pgx.Tx, p authz.Principal, d SAMLDefinition, rotate bool) (samlConfiguration, error) {
	c := samlConfiguration{SAMLDefinition: d, ServiceProviderID: s.access.Origin + "/api/v1/saml/metadata"}
	if err := p.Authorize(authz.Access); err != nil {
		return c, err
	}
	metadata, err := validateSAML(c)
	if err != nil {
		return c, err
	}
	c.EntityID = metadata.Entity.EntityID
	old, err := loadSAML(ctx, tx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	c.ID = old.ID
	if c.ID == "" {
		c.ID = ids.New()
	}
	c.ETag = ids.New()
	c.Certificate = old.Certificate
	if old.ID == "" || rotate {
		cert, err := s.rotateSAMLSigningKey(ctx, tx, c.ID)
		if err != nil {
			return c, err
		}
		c.Certificate = cert
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.saml_configuration(singleton,id,document,etag,updated_by) VALUES(true,$1,$2,$3,$4) ON CONFLICT(singleton) DO UPDATE SET document=excluded.document,etag=excluded.etag,updated_by=excluded.updated_by,updated_at=now()", c.ID, raw, c.ETag, p.UserID()); err != nil {
		return c, err
	}
	if err := s.invalidateSAMLAfterConfigChange(ctx, tx, old, c); err != nil {
		return c, err
	}
	return c, nil
}

func (s *samlService) importSAMLMetadata(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		URL string `json:"url"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if err := access.CheckIdentityURL(input.URL); err != nil {
		return httpkit.Reply{}, problem.Invalid("url", "Use a safe identity metadata URL.")
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, input.URL, nil)
	if err != nil {
		return httpkit.Reply{}, err
	}
	resp, err := s.access.IdentityClient().Do(req)
	if err != nil {
		return httpkit.Reply{}, problem.Invalid("url", "The metadata endpoint is unavailable or outside identity egress policy.")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, samlidentity.MaxXML+1))
	if err != nil || resp.StatusCode != http.StatusOK {
		return httpkit.Reply{}, problem.Invalid("url", "The metadata endpoint did not return a valid document.")
	}
	m, err := samlidentity.ParseMetadata(b)
	if err != nil || access.CheckIdentityURL(m.RedirectURL) != nil {
		return httpkit.Reply{}, problem.Invalid("url", "Invalid or unsupported SAML identity metadata.")
	}
	return httpkit.OK(samlImportResponse{MetadataXML: string(b), IDPEntityID: m.Entity.EntityID}), nil
}

func (s *samlService) samlMetadata(w http.ResponseWriter, r *http.Request, _ authz.Principal) error {
	c, err := loadSAML(r.Context(), s.access.Pool)
	if err != nil {
		return err
	}
	sp, _, err := s.samlProvider(r, s.access.Pool, c)
	if err != nil {
		return err
	}
	m := sp.Metadata()
	m.SPSSODescriptors[0].KeyDescriptors = slices.DeleteFunc(m.SPSSODescriptors[0].KeyDescriptors, func(k saml.KeyDescriptor) bool { return k.Use == "encryption" })
	m.SPSSODescriptors[0].SingleLogoutServices = nil
	raw, err := xml.Marshal(m)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, err = w.Write(raw)
	return err
}

func encodeSAMLCertificate(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
