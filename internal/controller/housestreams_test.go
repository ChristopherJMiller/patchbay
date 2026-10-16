package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// houseDoc is the page whose manifests the house copies into luma-homeops.
var houseDoc = filepath.Join("..", "..", "docs", "streams.md")

// yamlBlocks are the page's ```yaml blocks.
var yamlBlocks = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// houseManifests are docs/streams.md's house values and Streams: the
// block that sets streams.tunerPools, and the block of Streams that
// starts with the TV channels.
func houseManifests(t *testing.T) (values string, streamDocs []string) {
	t.Helper()
	raw, err := os.ReadFile(houseDoc)
	if err != nil {
		t.Fatal(err)
	}
	section := string(raw)
	if i := strings.Index(section, "## The house"); i >= 0 {
		section = section[i:]
	} else {
		t.Fatalf("%s has no section \"The house\"", houseDoc)
	}
	for _, m := range yamlBlocks.FindAllStringSubmatch(section, -1) {
		switch block := m[1]; {
		case strings.HasPrefix(block, "streamBridge:"):
			values = block
		case strings.Contains(block, "kind: Stream"):
			streamDocs = strings.Split(block, "\n---\n")
		}
	}
	if values == "" || len(streamDocs) == 0 {
		t.Fatalf("%s: the house section lacks its values or its Streams", houseDoc)
	}
	return values, streamDocs
}

// TestHouseStreams keeps the house's manifests in docs/streams.md (which
// the lead copies into luma-homeops) true: the values are the chart's
// shape and the settings they make validate; every Stream decodes
// strictly, passes the bridge's source rules and, in a real API server,
// the CRD's; and compiled with those settings the TV channels are in pool
// hdhomerun (max 1, the house's policy), the radio in none, with the 2 h
// limit in the dialplan.
func TestHouseStreams(t *testing.T) {
	valuesText, docs := houseManifests(t)
	var values struct {
		StreamBridge struct {
			Enabled bool `json:"enabled"`
		} `json:"streamBridge"`
		Streams struct {
			CallLimit  string              `json:"callLimit"`
			TunerPools []compile.TunerPool `json:"tunerPools"`
		} `json:"streams"`
	}
	if err := yaml.UnmarshalStrict([]byte(valuesText), &values); err != nil {
		t.Fatalf("the house values: %v\n%s", err, valuesText)
	}
	if !values.StreamBridge.Enabled {
		t.Error("the house values turn the stream bridge off")
	}
	limit, err := time.ParseDuration(values.Streams.CallLimit)
	if err != nil || limit != compile.DefaultCallLimit {
		t.Errorf("the house callLimit %q, want the 2h Chris chose (%v)", values.Streams.CallLimit, err)
	}
	wantPool := compile.TunerPool{Name: "hdhomerun", Hosts: []string{"192.168.0.58"}, Max: 1}
	if p := values.Streams.TunerPools; len(p) != 1 || p[0].Name != wantPool.Name || p[0].Max != 1 ||
		len(p[0].Hosts) != 1 || p[0].Hosts[0] != wantPool.Hosts[0] {
		t.Errorf("the house tunerPools %+v, want [%+v]", p, wantPool)
	}

	s, err := settings.Load(filepath.Join("settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Streams.CallLimit = metav1.Duration{Duration: limit}
	s.Streams.TunerPools = values.Streams.TunerPools
	if err := s.Validate(); err != nil {
		t.Fatalf("the house settings: %v", err)
	}

	var house []v1alpha1.Stream
	for _, d := range docs {
		var st v1alpha1.Stream
		if err := yaml.UnmarshalStrict([]byte(d), &st); err != nil {
			t.Fatalf("a house Stream: %v\n%s", err, d)
		}
		if st.Kind != "Stream" || st.APIVersion != v1alpha1.GroupVersion.String() {
			t.Errorf("%s is %s %s", st.Name, st.APIVersion, st.Kind)
		}
		if err := streams.CheckSource(st.Spec.Source); err != nil {
			t.Errorf("Stream %s: %v", st.Name, err)
		}
		house = append(house, st)
	}
	wantNumbers := []string{"704", "705", "707", "713", "750", "751", "752", "753", "754", "755"}
	if len(house) != len(wantNumbers) {
		t.Fatalf("%d house Streams, want %d", len(house), len(wantNumbers))
	}
	for i, st := range house {
		if string(st.Spec.Number) != wantNumbers[i] {
			t.Errorf("house Stream %d is %s on %s, want %s", i, st.Name, st.Spec.Number, wantNumbers[i])
		}
	}

	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	b, err := compile.Compile(reg, compile.Inputs{
		Streams: house,
		Secrets: map[compile.SecretRef][]byte{s.AMISecret: []byte("not-a-secret-ami")},
	}, s)
	if err != nil {
		t.Fatalf("compiling the house Streams: %v", err)
	}
	if len(b.Streams.Streams) != len(house) {
		t.Errorf("streams.json holds %d of the %d house Streams", len(b.Streams.Streams), len(house))
	}
	for _, st := range house {
		got, ok := b.Streams.Streams[streams.UUID(st.Name)]
		want := ""
		if strings.HasPrefix(st.Spec.Source, "http://192.168.0.58:5004/auto/v") {
			want = wantPool.Name
		}
		if !ok || got.Pool != want || got.Number != string(st.Spec.Number) {
			t.Errorf("Stream %s compiled as %+v (%v), want pool %q", st.Name, got, ok, want)
		}
	}
	if dialplan := string(b.Files[compile.TreeAsterisk+"/extensions.conf"]); !strings.Contains(dialplan, "Set(TIMEOUT(absolute)=7200)") {
		t.Error("the dialplan does not end stream calls at 2 h")
	}

	// The CRD's own rules (CEL on spec.source, the number's pattern).
	needEnvtest(t)
	ns := newNamespace(t)
	for _, st := range house {
		obj := st.DeepCopy()
		obj.Namespace = ns
		if err := testClient.Create(context.Background(), obj); err != nil {
			t.Errorf("the API server refuses house Stream %s: %v", st.Name, err)
		}
	}
}
