package access

import (
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
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/jackc/pgx/v5"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/tyk-swe/olp/internal/samlidentity"
	"github.com/tyk-swe/olp/internal/secrets"
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

func ValidateSAMLDefinition(d SAMLDefinition) error {
	_, err := validateSAML(samlConfiguration{SAMLDefinition: d})
	return err
}

type samlSigningKey struct {
	Private     []byte `json:"private"`
	Certificate []byte `json:"certificate"`
}

func loadSAML(r *http.Request, q Queryer) (samlConfiguration, error) {
	var c samlConfiguration
	err := q.QueryRow(r.Context(), "SELECT document||jsonb_build_object('id',id,'etag',etag) FROM olp.saml_configuration WHERE singleton").Scan(&c)
	return c, err
}

func (s *Server) samlConfiguration(r *http.Request, _ Principal) (Reply, error) {
	c, e := loadSAML(r, s.Pool)
	return Detail(c, c.ETag), e
}

func validateSAML(c samlConfiguration) (samlidentity.Metadata, error) {
	m, err := samlidentity.ParseMetadata([]byte(c.Metadata))
	if err != nil {
		return m, Invalid("metadata_xml", "Import one SAML 2.0 IdP descriptor with signing certificates and an HTTP-Redirect sign-in endpoint.")
	}
	if c.Enabled && !m.Entity.ValidUntil.IsZero() && !m.Entity.ValidUntil.After(time.Now()) {
		return m, Invalid("metadata_xml", "The imported identity metadata has expired.")
	}
	if c.Enabled && !slices.ContainsFunc(m.Certificates, func(cert *x509.Certificate) bool {
		now := time.Now()
		return !now.Before(cert.NotBefore) && !now.After(cert.NotAfter)
	}) {
		return m, Invalid("metadata_xml", "The identity metadata needs a currently valid signing certificate.")
	}
	if oidcURL(m.RedirectURL) != nil {
		return m, Invalid("metadata_xml", "The identity provider sign-in endpoint must use a safe HTTPS URL.")
	}
	for field, value := range map[string]string{"email_attribute": c.EmailAttribute, "groups_attribute": c.GroupsAttribute, "name_attribute": c.NameAttribute} {
		if value == "" && field != "email_attribute" {
			continue
		}
		if ValidText(field, value, 256) != nil {
			return m, Invalid(field, "Use a bounded SAML attribute name.")
		}
	}
	if c.DefaultRole != nil && !validRole(*c.DefaultRole) {
		return m, Invalid("default_role", "Choose a valid role.")
	}
	for field, list := range map[string][]roleMapping{"email_role_mappings": c.EmailMappings, "group_role_mappings": c.GroupMappings} {
		if len(list) > 500 {
			return m, Invalid(field, "Use at most 500 mappings.")
		}
		for i, v := range list {
			if !validRole(v.Role) || ValidText(field, v.ClaimValue, 254) != nil || slices.ContainsFunc(list[:i], func(other roleMapping) bool {
				return other.ClaimValue == v.ClaimValue || field == "email_role_mappings" && strings.EqualFold(other.ClaimValue, v.ClaimValue)
			}) {
				return m, Invalid(field, "Use unique values and valid roles.")
			}
		}
	}
	return m, nil
}

func samlMappedRole(c samlConfiguration, email string, groups []string) string {
	return mappedRole(oidcConfiguration{DefaultRole: c.DefaultRole, EmailMappings: c.EmailMappings, GroupMappings: c.GroupMappings}, email, groups)
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

func (s *Server) samlProvider(r *http.Request, q Queryer, c samlConfiguration) (*saml.ServiceProvider, samlidentity.Metadata, error) {
	m, err := validateSAML(c)
	if err != nil {
		return nil, m, err
	}
	raw, err := s.Keys.Read(r.Context(), q, s.Installation, c.ID, secrets.SAMLKey)
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
	metadataURL, _ := url.Parse(s.Origin + "/api/v1/saml/metadata")
	acs, _ := url.Parse(s.Origin + "/api/v1/saml/acs")
	sp := &saml.ServiceProvider{EntityID: s.Origin + "/api/v1/saml/metadata", Key: rsaKey, Certificate: cert, MetadataURL: *metadataURL, AcsURL: *acs, IDPMetadata: m.Entity, SignatureMethod: dsig.RSASHA256SignatureMethod, AuthnNameIDFormat: saml.PersistentNameIDFormat, HTTPClient: s.OIDCClient}
	return sp, m, nil
}

func (s *Server) putSAMLConfiguration(r *http.Request, _ Principal) (Reply, error) {
	var input struct {
		SAMLDefinition
		Rotate bool `json:"rotate_signing_key"`
	}
	if err := DecodeUnique(r, &input, 2<<20); err != nil {
		return Reply{}, err
	}
	if err := ValidateSAMLDefinition(input.SAMLDefinition); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	old, err := loadSAML(r, tx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	if old.ID != "" {
		if err = Match(r, old.ETag); err != nil {
			return Reply{}, err
		}
	} else if r.Header.Get("If-Match") != "" {
		return Reply{}, Fail(412, "etag_mismatch", "No SAML configuration exists.")
	}
	c, err := s.StoreSAMLDefinition(r, tx, p, input.SAMLDefinition, input.Rotate)
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(c, c.ETag))
}

// StoreSAMLDefinition is shared by owner management and configuration promotion.
// Callers hold the installation mutation lock and enforce their own precondition.
func (s *Server) StoreSAMLDefinition(r *http.Request, tx pgx.Tx, p Principal, d SAMLDefinition, rotate bool) (samlConfiguration, error) {
	c := samlConfiguration{SAMLDefinition: d, ServiceProviderID: s.Origin + "/api/v1/saml/metadata"}
	if err := p.Authorize(Access); err != nil {
		return c, err
	}
	metadata, err := validateSAML(c)
	if err != nil {
		return c, err
	}
	c.EntityID = metadata.Entity.EntityID
	old, err := loadSAML(r, tx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	c.ID = old.ID
	if c.ID == "" {
		c.ID = NewID()
	}
	c.ETag = NewID()
	c.Certificate = old.Certificate
	if old.ID == "" || rotate {
		key, e := newSAMLSigningKey()
		if e != nil {
			return c, e
		}
		raw, e := json.Marshal(key)
		if e != nil {
			return c, e
		}
		defer clear(raw)
		defer clear(key.Private)
		if e = s.Keys.Store(r.Context(), tx, s.Installation, c.ID, secrets.SAMLKey, raw, nil); e != nil {
			return c, e
		}
		c.Certificate = encodeSAMLCertificate(key.Certificate)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp.saml_configuration(singleton,id,document,etag,updated_by) VALUES(true,$1,$2,$3,$4) ON CONFLICT(singleton) DO UPDATE SET document=excluded.document,etag=excluded.etag,updated_by=excluded.updated_by,updated_at=now()`, c.ID, raw, c.ETag, p.UserID()); err != nil {
		return c, err
	}
	if old.EntityID != c.EntityID || old.EmailAttribute != c.EmailAttribute || old.GroupsAttribute != c.GroupsAttribute || rotate {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.saml_identities SET role_claims=NULL"); err != nil {
			return c, err
		}
	}
	if err = s.usableOwner(r, tx); err != nil {
		return c, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE purpose=$1", secrets.SAMLFlow); err != nil {
		return c, err
	}
	if !c.Enabled {
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE auth_method='saml'"); err != nil {
			return c, err
		}
	}

	if err = Audit(r.Context(), tx, r, p.Actor(), "saml.configuration.update", "saml_configuration", c.ID, "success"); err != nil {
		return c, err
	}
	return c, nil
}

func (s *Server) importSAMLMetadata(r *http.Request, _ Principal) (Reply, error) {
	var input struct {
		URL string `json:"url"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if err := oidcURL(input.URL); err != nil {
		return Reply{}, Invalid("url", "Use a safe identity metadata URL.")
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", input.URL, nil)
	if err != nil {
		return Reply{}, err
	}
	resp, err := s.OIDCClient.Do(req)
	if err != nil {
		return Reply{}, Invalid("url", "The metadata endpoint is unavailable or outside identity egress policy.")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, samlidentity.MaxXML+1))
	if err != nil || resp.StatusCode != 200 {
		return Reply{}, Invalid("url", "The metadata endpoint did not return a valid document.")
	}
	m, err := samlidentity.ParseMetadata(b)
	if err != nil || oidcURL(m.RedirectURL) != nil {
		return Reply{}, Invalid("url", "Invalid or unsupported SAML identity metadata.")
	}
	return OK(map[string]any{"metadata_xml": string(b), "idp_entity_id": m.Entity.EntityID}), nil
}

func (s *Server) samlMetadata(w http.ResponseWriter, r *http.Request, _ Principal) error {
	c, err := loadSAML(r, s.Pool)
	if err != nil {
		return err
	}
	sp, _, err := s.samlProvider(r, s.Pool, c)
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
