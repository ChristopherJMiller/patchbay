package compile_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestProjectedKey(t *testing.T) {
	for _, tc := range []struct {
		ref  compile.SecretRef
		want string
	}{
		{compile.PasswordRef("phone-desk"), "patchbay-phone-desk_sip-password"},
		{compile.ProvisioningTokenRef("softphone-chris-laptop"), "patchbay-softphone-chris-laptop_provisioning-token"},
		{compile.SecretRef{Name: "patchbay-ami", Key: "password"}, "patchbay-ami_password"},
		// A key may hold "_"; only the name may not.
		{compile.SecretRef{Name: "a.b", Key: "x_y"}, "a.b_x_y"},
	} {
		got, err := tc.ref.ProjectedKey()
		if err != nil || got != tc.want {
			t.Errorf("%+v.ProjectedKey() = %q, %v; want %q", tc.ref, got, err, tc.want)
			continue
		}
		back, err := compile.SecretRefFromProjectedKey(got)
		if err != nil || back != tc.ref {
			t.Errorf("SecretRefFromProjectedKey(%q) = %+v, %v; want %+v", got, back, err, tc.ref)
		}
	}
	for _, ref := range []compile.SecretRef{
		{Name: "", Key: "k"},
		{Name: "has_underscore", Key: "k"},
		{Name: "Upper", Key: "k"},
		{Name: "n", Key: ""},
		{Name: "n", Key: "a/b"},
		{Name: strings.Repeat("a", 250), Key: "key"}, // 254 characters projected
	} {
		if got, err := ref.ProjectedKey(); err == nil {
			t.Errorf("%+v.ProjectedKey() = %q, want an error", ref, got)
		}
	}
	for _, key := range []string{"nounderscore", "_k", "n_", "Bad_k", "n_a/b"} {
		if got, err := compile.SecretRefFromProjectedKey(key); err == nil {
			t.Errorf("SecretRefFromProjectedKey(%q) = %+v, want an error", key, got)
		}
	}
}

// TestSecretRefsBuiltin pins SecretRefs on the house golden: the two SIP
// passwords, the softphone's token and the AMI password, sorted, each a
// valid projected key.
func TestSecretRefsBuiltin(t *testing.T) {
	in, s := loadGolden(t, "house")
	b, err := compile.Compile(builtinRegistry(t), in, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.SecretRefs()
	if err != nil {
		t.Fatal(err)
	}
	want := []compile.SecretRef{
		s.AMISecret,
		s.PushSecret,
		compile.PasswordRef("phone-desk"),
		compile.PasswordRef("softphone-chris-laptop"),
		compile.ProvisioningTokenRef("softphone-chris-laptop"),
	}
	slices.SortFunc(want, func(a, b compile.SecretRef) int {
		return strings.Compare(a.Name+"\x00"+a.Key, b.Name+"\x00"+b.Key)
	})
	if !slices.Equal(got, want) {
		t.Fatalf("SecretRefs() = %+v\nwant %+v", got, want)
	}
	for _, r := range got {
		if _, err := r.ProjectedKey(); err != nil {
			t.Error(err)
		}
		if len(in.Secrets[r]) == 0 {
			t.Errorf("%+v is referenced but the golden inputs have no value for it", r)
		}
	}
	if idx := b.Index(); idx.Version != b.Version || len(idx.Devices) != len(b.Devices) || idx.AMI != b.AMI || idx.Push != b.Push {
		t.Errorf("Index() = %+v does not match the bundle", idx)
	}
}

func TestSecretRefsIncludePush(t *testing.T) {
	in, s := loadGolden(t, "house")
	b, err := compile.Compile(builtinRegistry(t), in, s)
	if err != nil {
		t.Fatal(err)
	}
	if b.Push != (compile.PushAccount{Username: compile.PushUsername, Password: s.PushSecret}) {
		t.Errorf("Push = %+v", b.Push)
	}
	refs, err := b.SecretRefs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(refs, b.Push.Password) {
		t.Errorf("SecretRefs() = %+v lacks the push password %+v", refs, b.Push.Password)
	}
}

func TestSecretRefsRefusesMalformedPlaceholders(t *testing.T) {
	b := compile.Bundle{Files: map[string][]byte{
		"tftp/x.xml": []byte("@@secret:Not_A_Name/k@@"),
	}}
	if refs, err := b.SecretRefs(); err == nil {
		t.Fatalf("SecretRefs() = %+v, want an error for a malformed placeholder", refs)
	}
}

// TestModules pins the module list: exactly modules.conf's load lines, in
// order, unique, and none of the modules that can run a command, which the
// chart hides from Asterisk by mounting only these over AsteriskModuleDir.
func TestModules(t *testing.T) {
	in, s := loadGolden(t, "empty")
	b, err := compile.Compile(builtinRegistry(t), in, s)
	if err != nil {
		t.Fatal(err)
	}
	var loaded []string
	for line := range bytes.Lines(b.Files[compile.TreeAsterisk+"/modules.conf"]) {
		if m, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "load => "); ok {
			loaded = append(loaded, m)
		}
	}
	mods := compile.Modules()
	if !slices.Equal(mods, loaded) {
		t.Fatalf("Modules() = %v\nmodules.conf loads %v", mods, loaded)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(mods)))) != len(mods) {
		t.Errorf("Modules() has duplicates: %v", mods)
	}
	for _, bad := range []string{"app_system.so", "func_shell.so", "res_agi.so", "res_ari.so", "app_exec.so", "res_http_websocket.so"} {
		if slices.Contains(mods, bad) {
			t.Errorf("Modules() lists %s", bad)
		}
	}
	if !bytes.Contains(b.Files[compile.TreeAsterisk+"/asterisk.conf"], []byte("astmoddir => "+compile.AsteriskModuleDir+"\n")) {
		t.Errorf("asterisk.conf does not name astmoddir %s", compile.AsteriskModuleDir)
	}
}
