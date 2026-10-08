package access

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/scim"
)

func scimAttribute(name, typ string, multi, required bool) map[string]any {
	return map[string]any{"name": name, "type": typ, "multiValued": multi, "required": required, "caseExact": name == "externalId" || name == "id", "mutability": "readWrite", "returned": "default", "uniqueness": "none"}
}

func scimSchemas() []map[string]any {
	text := func(name string, required bool) map[string]any { return scimAttribute(name, "string", false, required) }
	name := scimAttribute("name", "complex", false, false)
	name["subAttributes"] = []any{text("formatted", false), text("familyName", false), text("givenName", false), text("middleName", false), text("honorificPrefix", false), text("honorificSuffix", false)}
	email := scimAttribute("emails", "complex", true, false)
	email["subAttributes"] = []any{text("value", true), text("type", false), scimAttribute("primary", "boolean", false, false)}
	groups := scimAttribute("groups", "complex", true, false)
	groups["mutability"] = "readOnly"
	groups["subAttributes"] = []any{text("value", true), text("display", false), text("type", false), scimAttribute("$ref", "reference", false, false)}
	members := scimAttribute("members", "complex", true, false)
	members["subAttributes"] = []any{text("value", true), text("display", false), text("type", false), scimAttribute("$ref", "reference", false, false)}
	roles := scimAttribute("roles", "complex", true, false)
	roles["mutability"] = "readOnly"
	roles["subAttributes"] = []any{text("value", true), scimAttribute("primary", "boolean", false, false)}
	username := text("userName", true)
	username["uniqueness"] = "server"
	projects := scimAttribute("projects", "complex", true, false)
	projects["subAttributes"] = []any{text("value", true), text("role", true)}
	return []map[string]any{
		{"id": scim.UserSchema, "name": "User", "description": "Federated users; userName is a unique email address. Local passwords cannot be provisioned.", "attributes": []any{username, text("externalId", false), text("displayName", false), scimAttribute("active", "boolean", false, false), name, email, groups, roles}},
		{"id": scim.GroupSchema, "name": "Group", "description": "Flat groups containing SCIM users.", "attributes": []any{text("displayName", true), text("externalId", false), members}},
		{"id": scim.UserExtension, "name": "OLPUser", "description": "Base role and access scope. Group grants can add authority; manual local takeover stops reconciliation.", "attributes": []any{text("role", false), text("accessScope", false)}},
		{"id": scim.GroupExtension, "name": "OLPGroup", "description": "Additional role and scope, plus inherited project memberships. Removing a group preserves direct memberships.", "attributes": []any{text("role", false), text("accessScope", false), projects}},
	}
}

func (s *Server) scimDiscovery(r *http.Request, _ Principal) (Reply, error) {
	if strings.HasSuffix(r.URL.Path, "/ServiceProviderConfig") {
		return OK(map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"}, "patch": map[string]any{"supported": true}, "bulk": map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0}, "filter": map[string]any{"supported": true, "maxResults": 200}, "changePassword": map[string]any{"supported": false}, "sort": map[string]any{"supported": true}, "etag": map[string]any{"supported": true}, "authenticationSchemes": []any{map[string]any{"type": "oauthbearertoken", "name": "OLP management token", "description": "Installation-wide management token with access scope; creator authority is rechecked on every request.", "primary": true}}, "meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": s.Origin + "/scim/v2/ServiceProviderConfig"}}), nil
	}
	var resources []map[string]any
	if strings.Contains(r.URL.Path, "/Schemas") {
		resources = scimSchemas()
		for _, schema := range resources {
			schema["schemas"] = []string{"urn:ietf:params:scim:schemas:core:2.0:Schema"}
			schema["meta"] = map[string]any{"resourceType": "Schema", "location": s.Origin + "/scim/v2/Schemas/" + schema["id"].(string)}
		}
	} else {
		for _, kind := range []string{"User", "Group"} {
			schema, extension := scim.UserSchema, scim.UserExtension
			if kind == "Group" {
				schema, extension = scim.GroupSchema, scim.GroupExtension
			}
			resources = append(resources, map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": kind, "name": kind, "endpoint": "/" + kind + "s", "schema": schema, "schemaExtensions": []any{map[string]any{"schema": extension, "required": false}}, "meta": map[string]any{"resourceType": "ResourceType", "location": s.Origin + "/scim/v2/ResourceTypes/" + kind}})
		}
	}
	id := r.PathValue("schema_id")
	if id == "" {
		id = r.PathValue("scim_type")
	}
	if id != "" {
		for _, resource := range resources {
			if resource["id"] == id {
				return OK(resource), nil
			}
		}
		return Reply{}, scim.Fail(404, "", "The schema or resource type was not found.")
	}
	return OK(map[string]any{"schemas": []string{scim.ListSchema}, "totalResults": len(resources), "startIndex": 1, "itemsPerPage": len(resources), "Resources": resources}), nil
}
