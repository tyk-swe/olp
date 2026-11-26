package identity

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

type scimMeta struct {
	ResourceType string `json:"resourceType"`
	Location     string `json:"location"`
}

type scimSupported struct {
	Supported bool `json:"supported"`
}

type scimBulkConfig struct {
	Supported      bool `json:"supported"`
	MaxOperations  int  `json:"maxOperations"`
	MaxPayloadSize int  `json:"maxPayloadSize"`
}

type scimFilterConfig struct {
	Supported  bool `json:"supported"`
	MaxResults int  `json:"maxResults"`
}

type scimAuthScheme struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Primary     bool   `json:"primary"`
}

type scimServiceProviderConfig struct {
	Schemas               []string         `json:"schemas"`
	Patch                 scimSupported    `json:"patch"`
	Bulk                  scimBulkConfig   `json:"bulk"`
	Filter                scimFilterConfig `json:"filter"`
	ChangePassword        scimSupported    `json:"changePassword"`
	Sort                  scimSupported    `json:"sort"`
	ETag                  scimSupported    `json:"etag"`
	AuthenticationSchemes []scimAuthScheme `json:"authenticationSchemes"`
	Meta                  scimMeta         `json:"meta"`
}

type scimSchemaAttribute struct {
	Name          string                `json:"name"`
	Type          string                `json:"type"`
	MultiValued   bool                  `json:"multiValued"`
	Required      bool                  `json:"required"`
	CaseExact     bool                  `json:"caseExact"`
	Mutability    string                `json:"mutability"`
	Returned      string                `json:"returned"`
	Uniqueness    string                `json:"uniqueness"`
	SubAttributes []scimSchemaAttribute `json:"subAttributes,omitempty"`
}

type scimSchemaDef struct {
	Schemas     []string              `json:"schemas"`
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Attributes  []scimSchemaAttribute `json:"attributes"`
	Meta        scimMeta              `json:"meta"`
}

type scimSchemaExtensionRef struct {
	Schema   string `json:"schema"`
	Required bool   `json:"required"`
}

type scimResourceType struct {
	Schemas          []string                 `json:"schemas"`
	ID               string                   `json:"id"`
	Name             string                   `json:"name"`
	Endpoint         string                   `json:"endpoint"`
	Schema           string                   `json:"schema"`
	SchemaExtensions []scimSchemaExtensionRef `json:"schemaExtensions"`
	Meta             scimMeta                 `json:"meta"`
}

func scimAttribute(name, typ string, multi, required bool) scimSchemaAttribute {
	return scimSchemaAttribute{
		Name:        name,
		Type:        typ,
		MultiValued: multi,
		Required:    required,
		CaseExact:   name == "externalId" || name == "id",
		Mutability:  "readWrite",
		Returned:    "default",
		Uniqueness:  "none",
	}
}

func scimServiceProviderConfigFor(origin string) scimServiceProviderConfig {
	return scimServiceProviderConfig{
		Schemas:        []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		Patch:          scimSupported{Supported: true},
		Bulk:           scimBulkConfig{Supported: false, MaxOperations: 0, MaxPayloadSize: 0},
		Filter:         scimFilterConfig{Supported: true, MaxResults: 200},
		ChangePassword: scimSupported{Supported: false},
		Sort:           scimSupported{Supported: true},
		ETag:           scimSupported{Supported: true},
		AuthenticationSchemes: []scimAuthScheme{
			{
				Type:        "oauthbearertoken",
				Name:        "OLP management token",
				Description: "Installation-wide management token with access scope; creator authority is rechecked on every request.",
				Primary:     true,
			},
		},
		Meta: scimMeta{
			ResourceType: "ServiceProviderConfig",
			Location:     origin + "/scim/v2/ServiceProviderConfig",
		},
	}
}

func scimSchemasFor(origin string) []scimSchemaDef {
	text := func(name string, required bool) scimSchemaAttribute {
		return scimAttribute(name, "string", false, required)
	}
	complexAttribute := func(name string, mutability string, subAttributes []scimSchemaAttribute) scimSchemaAttribute {
		attr := scimAttribute(name, "complex", scim.MultiValuedComplexAttribute(name), false)
		attr.Mutability = mutability
		attr.SubAttributes = subAttributes
		return attr
	}
	nameSub := slices.Collect(func(yield func(scimSchemaAttribute) bool) {
		for _, field := range scim.NameSubattributes() {
			if !yield(text(field, false)) {
				return
			}
		}
	})
	name := complexAttribute("name", "readWrite", nameSub)
	email := complexAttribute("emails", "readWrite", []scimSchemaAttribute{
		text("value", true),
		text("type", false),
		scimAttribute("primary", "boolean", false, false),
	})
	refSub := []scimSchemaAttribute{
		text("value", true),
		text("display", false),
		text("type", false),
		scimAttribute("$ref", "reference", false, false),
	}
	groups := complexAttribute("groups", "readOnly", refSub)
	members := complexAttribute("members", "readWrite", refSub)
	roles := complexAttribute("roles", "readOnly", []scimSchemaAttribute{
		text("value", true),
		scimAttribute("primary", "boolean", false, false),
	})
	username := text("userName", true)
	username.Uniqueness = "server"
	projects := complexAttribute("projects", "readWrite", []scimSchemaAttribute{
		text("value", true),
		text("role", true),
	})
	schemaDef := func(id, schemaName, description string, attrs []scimSchemaAttribute) scimSchemaDef {
		return scimSchemaDef{
			Schemas:     []string{"urn:ietf:params:scim:schemas:core:2.0:Schema"},
			ID:          id,
			Name:        schemaName,
			Description: description,
			Attributes:  attrs,
			Meta: scimMeta{
				ResourceType: "Schema",
				Location:     origin + "/scim/v2/Schemas/" + id,
			},
		}
	}
	return []scimSchemaDef{
		schemaDef(scim.UserSchema, "User", "Federated users; userName is a unique email address. Local passwords cannot be provisioned.", []scimSchemaAttribute{
			username, text("externalId", false), text("displayName", false), scimAttribute("active", "boolean", false, false), name, email, groups, roles,
		}),
		schemaDef(scim.GroupSchema, "Group", "Flat groups containing SCIM users.", []scimSchemaAttribute{
			text("displayName", true), text("externalId", false), members,
		}),
		schemaDef(scim.UserExtension, "OLPUser", "Base role and access scope. Group grants can add authority; manual local takeover stops reconciliation.", []scimSchemaAttribute{
			text("role", false), text("accessScope", false),
		}),
		schemaDef(scim.GroupExtension, "OLPGroup", "Additional role and scope, plus inherited project memberships. Removing a group preserves direct memberships.", []scimSchemaAttribute{
			text("role", false), text("accessScope", false), projects,
		}),
	}
}

func scimResourceTypesFor(origin string) []scimResourceType {
	build := func(kind, schema, extension string) scimResourceType {
		return scimResourceType{
			Schemas:          []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"},
			ID:               kind,
			Name:             kind,
			Endpoint:         "/" + kind + "s",
			Schema:           schema,
			SchemaExtensions: []scimSchemaExtensionRef{{Schema: extension, Required: false}},
			Meta: scimMeta{
				ResourceType: "ResourceType",
				Location:     origin + "/scim/v2/ResourceTypes/" + kind,
			},
		}
	}
	return []scimResourceType{
		build("User", scim.UserSchema, scim.UserExtension),
		build("Group", scim.GroupSchema, scim.GroupExtension),
	}
}

func scimDiscoveryReply[T any](items []T, id string, itemID func(T) string) (httpkit.Reply, error) {
	if id != "" {
		for _, item := range items {
			if itemID(item) == id {
				return httpkit.OK(item), nil
			}
		}
		return httpkit.Reply{}, scim.Fail(404, "", "The schema or resource type was not found.")
	}
	return httpkit.OK(scimListResponse[T]{
		Schemas:      []string{scim.ListSchema},
		TotalResults: int64(len(items)),
		StartIndex:   1,
		ItemsPerPage: len(items),
		Resources:    items,
	}), nil
}

func (s *scimService) scimDiscovery(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	if strings.HasSuffix(r.URL.Path, "/ServiceProviderConfig") {
		return httpkit.OK(scimServiceProviderConfigFor(s.access.Origin)), nil
	}
	id := cmp.Or(r.PathValue("schema_id"), r.PathValue("scim_type"))
	if strings.Contains(r.URL.Path, "/Schemas") {
		return scimDiscoveryReply(scimSchemasFor(s.access.Origin), id, func(schema scimSchemaDef) string { return schema.ID })
	}
	return scimDiscoveryReply(scimResourceTypesFor(s.access.Origin), id, func(rt scimResourceType) string { return rt.ID })
}
