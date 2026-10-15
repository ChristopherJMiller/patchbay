package builtin_test

import (
	"slices"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

func TestRegistry(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.Models(), []string{"cisco-7945", "cisco-7965", "cisco-7975", "softphone"}; !slices.Equal(got, want) {
		t.Errorf("Models() = %v, want %v", got, want)
	}
	var names []string
	for _, d := range reg.Drivers() {
		names = append(names, d.Name())
	}
	if want := []string{cisco79x5.Name, softphone.Name}; !slices.Equal(names, want) {
		t.Errorf("drivers %v, want %v in that order", names, want)
	}
	for _, m := range reg.Models() {
		if _, err := reg.ForModel(m); err != nil {
			t.Errorf("ForModel(%q): %v", m, err)
		}
	}
}
