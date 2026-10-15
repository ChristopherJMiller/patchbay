package v1alpha1_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

func TestMACNormalized(t *testing.T) {
	for _, tc := range []struct {
		in   v1alpha1.MAC
		want string
	}{
		{"D0C78915E8C8", "D0C78915E8C8"},
		{"d0c78915e8c8", "D0C78915E8C8"},
		{"d0C78915e8C8", "D0C78915E8C8"},
	} {
		if got := tc.in.Normalized(); got != tc.want {
			t.Errorf("MAC(%q).Normalized() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	for _, kind := range []string{
		"Phone", "PhoneList",
		"Softphone", "SoftphoneList",
		"Person", "PersonList",
		"Stream", "StreamList",
	} {
		if !s.Recognizes(v1alpha1.GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognise %s", kind)
		}
	}
}
