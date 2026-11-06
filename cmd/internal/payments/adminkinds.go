package payments

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	bdomain "github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// SubdriftKind is the operator's request to sweep one provider account's subscriptions against the provider (R110a): the one operation kind
// that acts on something the engine does not know of, so it is defined here and handed to the engine, which imports no billing. The
// arguments are an account id of the form a provider account can be called, and nothing else.
func SubdriftKind() app.AdminKind {
	return app.AdminKind{Kind: ports.AdminSubdrift, Parse: func(_ string, raw json.RawMessage) (json.RawMessage, error) {
		var in struct {
			Account string `json:"account"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("%w: a subdrift request is {\"account\": \"...\"}: %w", domain.ErrInvalid, err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: a subdrift request is one JSON object", domain.ErrInvalid)
		}
		if err := bdomain.ValidAccountID(in.Account); err != nil {
			return nil, err
		}
		return json.Marshal(in)
	}}
}
