package domain

import (
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// Billing's RFC 9457 problem types (LLD §5.1, §17): one platform-gincommon
// problem.Type per catalogue code, built once with problem.MustType, type
// URI https://errors.bcbpsolutions.com/commercial/<code-in-kebab-case>.
// platform-gincommon owns everything else: the problem model, the
// application/problem+json serialisation, traceId / instance / tenantId
// enrichment, validation formatting and error logging.

// ProblemDomain is the type-URI domain of Billing's problems.
const ProblemDomain = "commercial"

var problemTypes = func() map[ErrorCode]problem.Type {
	out := map[ErrorCode]problem.Type{}
	for code, status := range ErrorCatalogue() {
		name := strings.ReplaceAll(strings.ToLower(string(code)), "_", "-")
		s := strings.ReplaceAll(strings.ToLower(string(code)), "_", " ")
		out[code] = problem.MustType(ProblemDomain, name, strings.ToUpper(s[:1])+s[1:], status, string(code))
	}
	return out
}()

// ProblemType returns the problem type of code (false for an unknown code).
func ProblemType(code ErrorCode) (problem.Type, bool) {
	t, ok := problemTypes[code]
	return t, ok
}

// Problem is the error as a platform-gincommon problem: its catalogue type,
// message as detail, extension members, and the error as cause. INTERNAL or
// an unknown code is gincommon's 500, whose detail never reaches the client.
func (e *DomainError) Problem() *problem.Error {
	t, ok := problemTypes[e.Code]
	if e.Code == ErrInternal || !ok {
		return problem.NewInternalError(e).WithCode(string(ErrInternal))
	}
	return problem.NewError(t, e.Message).WithCause(e).WithExtensions(e.Extensions)
}

// As lets errors.As — and so gincommon.ClassifyError / AbortWithProblem —
// see a DomainError as a *problem.Error, so handlers hand domain errors to
// gincommon unchanged.
func (e *DomainError) As(target any) bool {
	if p, ok := target.(**problem.Error); ok {
		*p = e.Problem()
		return true
	}
	return false
}
