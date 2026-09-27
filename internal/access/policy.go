package access

// Operation is a management action that authorization decides. Delegable
// operations are also the scopes a management token may carry; the others are
// performed only by a signed-in member.
type Operation uint8

const (
	// Read reads configuration, keys, routes, and other project resources.
	Read Operation = iota + 1
	// Usage reads accounting, request history, and usage reports.
	Usage
	// Keys manages gateway API keys, budget groups, and notification rules.
	Keys
	// Playground runs inference through the management playground.
	Playground
	// Configure manages providers, routes, and media jobs.
	Configure
	// Settings manages installation settings and pricing.
	Settings
	// AccessRead reads installation membership, invitations, and identity.
	AccessRead
	// Access manages installation membership, invitations, and identity.
	Access
	// Self manages the signed-in member's own profile, sessions, and
	// credentials.
	Self
	// ManageSessions lists and revokes other members' sessions.
	ManageSessions
	// ManageTokens administers management tokens.
	ManageTokens
	// ManageProjects administers projects and their membership.
	ManageProjects
	// LocalLogin changes whether local password sign-in is available.
	LocalLogin
	operationCount
)

// Installation roles as stored on members.
const (
	RoleOwner     = "owner"
	RoleOperator  = "operator"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"
)

type roleSet uint8

const (
	owner roleSet = 1 << iota
	operator
	developer
	viewer
	everyone = owner | operator | developer | viewer
)

func (s roleSet) has(role string) bool {
	switch role {
	case RoleOwner:
		return s&owner != 0
	case RoleOperator:
		return s&operator != 0
	case RoleDeveloper:
		return s&developer != 0
	case RoleViewer:
		return s&viewer != 0
	}
	return false
}

type rule struct {
	name  string
	roles roleSet
	// installation requires an installation-wide access scope: a global
	// member, or an all-projects token whose creator is global.
	installation bool
	// delegable operations may be granted to management tokens.
	delegable bool
}

// policy is the whole management authorization table.
var policy = [operationCount]rule{
	Read:           {name: "read", roles: everyone, delegable: true},
	Usage:          {name: "usage", roles: everyone, delegable: true},
	Keys:           {name: "keys", roles: owner | operator | developer, delegable: true},
	Playground:     {name: "playground", roles: owner | operator | developer, delegable: true},
	Configure:      {name: "configure", roles: owner | operator, delegable: true},
	Settings:       {name: "settings", roles: owner | operator, installation: true, delegable: true},
	AccessRead:     {name: "access_read", roles: owner | operator, installation: true, delegable: true},
	Access:         {name: "access", roles: owner, installation: true, delegable: true},
	Self:           {name: "self", roles: everyone},
	ManageSessions: {name: "manage_sessions", roles: owner, installation: true},
	ManageTokens:   {name: "manage_tokens", roles: owner, installation: true},
	ManageProjects: {name: "manage_projects", roles: owner, installation: true},
	LocalLogin:     {name: "local_login", roles: owner, installation: true},
}

func (op Operation) rule() (rule, bool) {
	if op == 0 || op >= operationCount {
		return rule{}, false
	}
	return policy[op], true
}

func (op Operation) String() string {
	if r, ok := op.rule(); ok {
		return r.name
	}
	return "unknown"
}

// ParseOperation returns the operation with the given name.
func ParseOperation(name string) (Operation, bool) {
	for op := Read; op < operationCount; op++ {
		if policy[op].name == name {
			return op, true
		}
	}
	return 0, false
}

// Operations lists every operation.
func Operations() []Operation {
	ops := make([]Operation, 0, operationCount-1)
	for op := Read; op < operationCount; op++ {
		ops = append(ops, op)
	}
	return ops
}

// TokenScopes lists the operations a management token may carry.
func TokenScopes() []Operation {
	var ops []Operation
	for _, op := range Operations() {
		if policy[op].delegable {
			ops = append(ops, op)
		}
	}
	return ops
}

func validRole(role string) bool { return everyone.has(role) }

type operationSet uint32

func (s operationSet) has(op Operation) bool { return s&(1<<op) != 0 }

// Authorize reports whether p may perform op. A member holds an operation
// through their role; a management token holds it only when the operation is
// delegable, in the token's scopes, and held by the token's creator now.
func (p Principal) Authorize(op Operation) error {
	r, ok := op.rule()
	if !ok {
		return Forbidden()
	}
	role := p.Role
	if p.Kind == "machine" {
		if !r.delegable || !p.scopes.has(op) {
			return Forbidden()
		}
		role = p.creatorRole
	}
	if !r.roles.has(role) || r.installation && !p.AllProjects {
		return Forbidden()
	}
	return nil
}

// Operations lists every operation p may perform, which the console uses to
// show only what the server would admit.
func (p Principal) Operations() []Operation {
	var ops []Operation
	for _, op := range Operations() {
		if p.Authorize(op) == nil {
			ops = append(ops, op)
		}
	}
	return ops
}

// Actor is who an audit record attributes an action to: a member, a
// management token, or the installation itself.
type Actor struct {
	user, token string
}

// System attributes an action to the installation rather than a principal.
var System Actor

// UserActor attributes an action to a member.
func UserActor(id string) Actor { return Actor{user: id} }

// Actor attributes p's actions to the member or management token that made
// the request.
func (p Principal) Actor() Actor {
	if p.Kind == "machine" {
		return Actor{token: p.ID}
	}
	return Actor{user: p.ID}
}
