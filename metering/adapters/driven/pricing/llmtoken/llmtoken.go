// Package llmtoken is the reference pricer for LLM token metering: it validates the unit
// vocabulary, then delegates to the table pricer. The kernel knows none of these units; this
// package is the product-specific binding.
package llmtoken

import (
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// The units this pricer meters. Cached input tokens are priced by their own unit at their own
// rate, so a gateway must report them SEPARATELY from input tokens, not inside them.
//
//nolint:gosec // G101 false positive: "token" is an LLM token unit, not a credential.
const (
	InputToken  domain.Unit = "llm_input_token"
	CachedToken domain.Unit = "llm_cached_token"
	OutputToken domain.Unit = "llm_output_token"
)

// Name is the registry name a rate card selects this pricer by.
const Name = "llmtoken"

// Pricer is the llmtoken pricer.
type Pricer = pricing.Vocabulary

// New returns the pricer, bounded by limit (0 = the default 2^50).
func New(limit domain.Credits) Pricer {
	return pricing.NewVocabulary(Name, table.New(limit), InputToken, CachedToken, OutputToken)
}
