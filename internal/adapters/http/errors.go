package http

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// problemDomain is the type-URI domain of Billing's problems
// (https://errors.bcbpsolutions.com/commercial/<name>, §5.4 B-P4 example).
const problemDomain = "commercial"

// problemTypes holds one problem type per §17 code, built once.
var problemTypes = func() map[domain.ErrorCode]problem.Type {
	out := map[domain.ErrorCode]problem.Type{}
	for code, status := range domain.ErrorCatalogue() {
		name := strings.ReplaceAll(strings.ToLower(string(code)), "_", "-")
		out[code] = problem.MustType(problemDomain, name, titleFor(code), status, string(code))
	}
	return out
}()

// titleFor turns PLAN_CHANGE_BLOCKED into "Plan change blocked".
func titleFor(code domain.ErrorCode) string {
	s := strings.ReplaceAll(strings.ToLower(string(code)), "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// toProblem maps a use-case error onto an RFC 9457 problem (§5.1, §17).
// Domain errors keep their frozen errorCode, message and extension members;
// a build-phase stub is 501; anything unclassified is gincommon's 500 with
// no detail, so error text never reaches the client.
func toProblem(err error) *problem.Error {
	var pe *problem.Error
	if errors.As(err, &pe) {
		return pe
	}
	var ni *app.NotImplementedError
	if errors.As(err, &ni) {
		return problem.NewNotImplementedError(ni.Error())
	}
	var de *domain.DomainError
	if errors.As(err, &de) {
		if de.Code == domain.ErrInternal || !de.Code.Known() {
			return problem.NewInternalError(err).WithCode(string(domain.ErrInternal))
		}
		out := problem.NewError(problemTypes[de.Code], de.Message).WithCause(err)
		return out.WithExtensions(de.Extensions)
	}
	return gincommon.ClassifyError(err)
}

// writeError renders err and aborts the chain, adding Retry-After where §5.1
// requires it (409 REQUEST_IN_PROGRESS, 503).
func writeError(c *gin.Context, err error) {
	p := toProblem(err)
	switch {
	case p.ErrorCode() == string(domain.ErrRequestInProgress):
		c.Header("Retry-After", "2")
	case p.Status() == 503:
		c.Header("Retry-After", "5")
	}
	gincommon.AbortWithProblem(c, p)
}

// validationError is a 400 VALIDATION_ERROR naming one field. gincommon's
// validationErrors entries carry field and message only.
func validationError(field, message string) *problem.Error {
	return problem.NewValidationError(problem.FieldError{Field: field, Message: message}).
		WithCode(string(domain.ErrValidation))
}
