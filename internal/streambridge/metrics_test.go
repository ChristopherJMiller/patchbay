package streambridge

import (
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// gauge is the value of the sample of family name whose labels are labels
// ("k=v,k=v", sorted; "" for none), or -1 if there is none.
func gauge(t *testing.T, g prometheus.Gatherer, name, labels string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, s := range f.GetMetric() {
			var ls []string
			for _, l := range s.GetLabel() {
				ls = append(ls, l.GetName()+"="+l.GetValue())
			}
			slices.Sort(ls)
			if strings.Join(ls, ",") == labels {
				return s.GetCounter().GetValue() + s.GetGauge().GetValue()
			}
		}
	}
	return -1
}
