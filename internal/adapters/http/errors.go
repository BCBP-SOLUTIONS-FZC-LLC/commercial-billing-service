package http

import (
	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// writeError hands err to platform-gincommon, which owns the RFC 9457
// response (§5.1, §17): domain and not-implemented errors classify
// themselves as gincommon problems (errors.As), anything else is gincommon's
// own classification. The only addition is the Retry-After header §5.1
// requires on 409 REQUEST_IN_PROGRESS and 503 (gincommon has no header hook).
func writeError(c *gin.Context, err error) {
	p := gincommon.ClassifyError(err)
	switch {
	case p.ErrorCode() == string(domain.ErrRequestInProgress):
		c.Header("Retry-After", "2")
	case p.Status() == 503:
		c.Header("Retry-After", "5")
	}
	gincommon.AbortWithProblem(c, p)
}

// validationError is gincommon's 400 validation problem naming one field,
// with Billing's §17 code.
func validationError(field, message string) *problem.Error {
	return problem.NewValidationError(problem.FieldError{Field: field, Message: message}).
		WithCode(string(domain.ErrValidation))
}
