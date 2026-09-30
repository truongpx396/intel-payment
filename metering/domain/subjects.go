package domain

import (
	"fmt"
	"sort"
)

// SubjectJob is the one subject kind the engine knows: it keys a Window: Job budget.
const SubjectJob = "job"

// Subjects are the attribution dimensions of one call WITHIN its charged scope: the member who
// made it, the API key, the project, the job. They key subject-level ceilings ("user u1's daily
// budget inside org o1"), land on the ledger row for breakdowns, and are never a cost input.
// Kinds are host-defined, except "job".
type Subjects map[string]string // kind → id: {"user": "u1", "api_key": "k7", "job": "run_42"}

// Validate refuses empty kinds and ids: a subject counter keyed on "" would merge everyone's spend.
func (s Subjects) Validate() error {
	for k, v := range s {
		if k == "" || v == "" {
			return fmt.Errorf("%w: subject kind and id must be non-empty", ErrInvalid)
		}
	}
	return nil
}

// Kinds returns the subject kinds in a stable order.
func (s Subjects) Kinds() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
