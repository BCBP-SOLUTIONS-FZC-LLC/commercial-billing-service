package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

var taxIDPattern = regexp.MustCompile(`^[A-Z0-9-]+$`)

func init() {
	if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
		// tax_id: ≤ 64 of [A-Z0-9-] (§5.4.1 B-P26).
		_ = v.RegisterValidation("taxid", func(fl validator.FieldLevel) bool { return taxIDPattern.MatchString(fl.Field().String()) })
	}
}

// decodeJSON reads the body into dst and validates it (§10.4). strict
// rejects unknown fields (tenant and operator routes); B-I1 tolerates them.
// An empty body decodes as {}. Errors are 400 VALIDATION_ERROR (413 over the
// body limit).
func decodeJSON(c *gin.Context, dst any, strict bool) error {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return problem.NewError(problem.TypePayloadTooLarge, "request body too large").WithCode(string(domain.ErrPayloadTooLarge))
		}
		return validationError("body", "could not be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &typeErr):
			return validationError(typeErr.Field, "has the wrong type")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return validationError(field, "is not allowed")
		default:
			return validationError("body", "must be a JSON object")
		}
	}
	if dec.More() {
		return validationError("body", "must contain a single JSON object")
	}
	return validate(dst)
}

// validate runs the binding tags.
func validate(dst any) error {
	if err := binding.Validator.ValidateStruct(dst); err != nil {
		return gincommon.BindingError(err).WithCode(string(domain.ErrValidation))
	}
	return nil
}

// bindQuery binds and validates query parameters.
func bindQuery(c *gin.Context, dst any) error {
	if err := c.ShouldBindQuery(dst); err != nil {
		return gincommon.BindingError(err).WithCode(string(domain.ErrValidation))
	}
	return nil
}

// pathUUID returns the path parameter name, which must be a UUID.
func pathUUID(c *gin.Context, name string) (string, error) {
	v := c.Param(name)
	if _, err := uuid.Parse(v); err != nil || len(v) != 36 {
		return "", validationError(name, "must be a UUID")
	}
	return strings.ToLower(v), nil
}
