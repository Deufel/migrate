package migrate_test

import (
	"testing"

	"github.com/Deufel/sitegen"
)

// TestDocumented — the site is a by-product of the source: every package
// and exported name carries its comment, every function its example.
func TestDocumented(t *testing.T) {
	if err := sitegen.Must(sitegen.Check(".")); err != nil {
		t.Fatal(err)
	}
}
