package problem

type Problem struct {
	Status              int
	Code, Detail, Field string
}

func (p *Problem) Error() string { return p.Code }

func Fail(status int, code, detail string) error {
	return &Problem{Status: status, Code: code, Detail: detail}
}

func Invalid(field, detail string) error {
	return &Problem{Status: 422, Code: "validation_failed", Detail: detail, Field: field}
}

func Forbidden() error {
	return Fail(403, "permission_denied", "The current role cannot perform this operation.")
}

var NotFound = Fail(404, "not_found", "The resource was not found.")
