package sqlite_test

import (
	"testing"

	webhookd "github.com/NimbusNexus/webhookd-go"
	"github.com/NimbusNexus/webhookd-go/store/sqlite"
	"github.com/NimbusNexus/webhookd-go/store/storetest"
)

// SQLite runs the full contract locally — the driver is pure Go and ":memory:" needs no server.
func TestSqliteStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) webhookd.Store {
		s, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatalf("sqlite.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
