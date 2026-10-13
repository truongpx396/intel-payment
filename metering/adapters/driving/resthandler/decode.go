package resthandler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// bodyTooLarge is a request body over the cap: 413, never read further.
type bodyTooLarge struct{ limit int64 }

func (e bodyTooLarge) Error() string {
	return fmt.Sprintf("the request body is over the %d-byte limit", e.limit)
}

// decodeBody reads the request's JSON object into v, strictly: an unknown field is a typo that would
// silently not apply (and a ceiling that silently does not apply is the worst kind), and a number that is
// not an integer where an integer is meant — a fraction, an exponent, a string, past int64 — is not an
// amount. Anything wrong is ErrInvalid and names what.
func decodeBody(r *http.Request, v any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return bodyTooLarge{limit: tooLarge.Limit}
		}
		return fmt.Errorf("%w: the request body could not be read", domain.ErrInvalid)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return fmt.Errorf("%w: a JSON request body is required", domain.ErrInvalid)
	}
	if raw[0] != '{' {
		return fmt.Errorf("%w: the request body must be a JSON object", domain.ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return jsonError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON object", domain.ErrInvalid)
	}
	return nil
}

func jsonError(err error) error {
	var typeErr *json.UnmarshalTypeError
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "the body"
		}
		return fmt.Errorf("%w: %s must be %s, got a JSON %s", domain.ErrInvalid, field, expected(typeErr.Type), typeErr.Value)
	case errors.As(err, &syntax):
		return fmt.Errorf("%w: malformed JSON at byte %d", domain.ErrInvalid, syntax.Offset)
	}
	// "json: unknown field \"maxcost\"" and the like: the text names the field.
	return fmt.Errorf("%w: %s", domain.ErrInvalid, err.Error())
}

func expected(t interface{ String() string }) string {
	switch t.String() {
	case "int64":
		return "an integer"
	case "float64":
		return "a number"
	case "string":
		return "a string"
	}
	return t.String()
}
