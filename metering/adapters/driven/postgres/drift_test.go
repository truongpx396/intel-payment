//go:build integration

package postgres_test

import (
	"context"
	"testing"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a against PostgreSQL 16: the same contract the in-memory reader honours, over runs written by the writer's own SaveReconcile, with a
// database of its own per case (a case that counts runs must not see another's).
func TestPostgresDriftReaderHonoursTheContract(t *testing.T) {
	t.Parallel()
	contracts.DriftContract(t, func(tb testing.TB) contracts.DriftWorld {
		p := db(tb.(*testing.T))
		books := pgadapter.NewBooks(p)
		return contracts.DriftWorld{
			Reader: pgadapter.NewDrift(p),
			Save: func(tb testing.TB, r ports.ReconcileRecord) {
				tb.Helper()
				if err := books.SaveReconcile(context.Background(), r); err != nil {
					tb.Fatalf("save a reconcile run: %v", err)
				}
			},
		}
	})
}
