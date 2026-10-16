package bundle_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// streamSettings are settings() with the stream bridge on and the house's
// tuner pool (docs/plans/release-b.md §12).
func streamSettings() compile.Settings {
	s := settings()
	s.StreamBridge = install.StreamBridgeAddress(names, "telephony", 9092)
	s.Streams.TunerPools = []compile.TunerPool{{Name: "hdhomerun", Hosts: []string{"192.168.0.58"}, Max: 1}}
	return s
}

func (f *fixture) newStream(t *testing.T, name, number, source string) {
	t.Helper()
	f.create(t, &v1alpha1.Stream{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.StreamSpec{Number: v1alpha1.Number(number), Name: strings.ToUpper(name), Source: source},
	})
}

func (f *fixture) streamsConfigMap(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: names.StreamsConfigMap}, &cm); err != nil {
		t.Fatalf("%s: %v", names.StreamsConfigMap, err)
	}
	return &cm
}

// streamsDoc reads <fullname>-streams back as the controller does and
// checks its metadata.
func (f *fixture) streamsDoc(t *testing.T) streams.Document {
	t.Helper()
	d, err := install.ReadStreams(context.Background(), f.c, f.ns, names)
	if err != nil {
		t.Fatalf("ReadStreams: %v", err)
	}
	cm := f.streamsConfigMap(t)
	if cm.Annotations[install.AnnotationStreamsVersion] != d.Version {
		t.Errorf("streams-version %q, want the document's %s", cm.Annotations[install.AnnotationStreamsVersion], d.Version)
	}
	for k, v := range install.ManagedLabels() {
		if cm.Labels[k] != v {
			t.Errorf("%s label %s = %q, want %q", cm.Name, k, cm.Labels[k], v)
		}
	}
	if keys := slices.Sorted(maps.Keys(cm.Data)); !slices.Equal(keys, []string{install.StreamsKey}) || len(cm.BinaryData) != 0 {
		t.Errorf("%s keys %v, want only %s", cm.Name, keys, install.StreamsKey)
	}
	return d
}

// TestWritesStreamsFirst: <fullname>-streams is written before pbx-secrets
// and pbx-config, so the bridge's file holds a stream before the dialplan
// routes its number; ConfigWritten counts the streams.
func TestWritesStreamsFirst(t *testing.T) {
	f := newFixtureWith(t, streamSettings())
	f.site(t)
	f.newStream(t, "king5", "705", "http://192.168.0.58:5004/auto/v5.1")
	f.newStream(t, "groovesalad", "750", "https://ice2.somafm.com/groovesalad-128-mp3")
	f.reconcile(t)
	ops := f.w.take()
	st, sec, cm := indexOf(ops, "create ConfigMap/"+names.StreamsConfigMap), indexOf(ops, "create Secret/"+names.ProjectedSecret),
		indexOf(ops, "create ConfigMap/"+names.ConfigMap)
	if st < 0 || sec < 0 || cm < 0 || st > sec || sec > cm {
		t.Fatalf("writes %v: want %s, then pbx-secrets, then pbx-config", ops, names.StreamsConfigMap)
	}
	d := f.streamsDoc(t)
	king, ok := d.Streams[streams.UUID("king5")]
	if !ok || king.Pool != "hdhomerun" || king.Number != "705" || len(d.Streams) != 2 ||
		!slices.Equal(d.Pools, []streams.Pool{{Name: "hdhomerun", Max: 1}}) {
		t.Errorf("streams.json %+v", d)
	}
	b, _ := f.compiled(t)
	if !strings.Contains(string(b.Files["asterisk/extensions.conf"]), "Dial(AudioSocket/"+streamSettings().StreamBridge+"/"+streams.UUID("king5")) {
		t.Error("pbx-config does not route 705 to the bridge")
	}
	evs := f.rec.take()
	if len(evs) != 1 || evs[0].reason != install.EventReasonConfigWritten || !strings.HasSuffix(evs[0].note, ", 2 devices, 2 streams") {
		t.Errorf("Events %+v, want one ConfigWritten counting 2 streams", evs)
	}
	f.noSecretsLeaked(t, evs)
}

// TestStreamsWithoutStreams: a first compile with no Streams and no stream
// bridge still writes an empty <fullname>-streams, which the bridge pod
// mounts (not optional) and the Stream reconciler waits for.
func TestStreamsWithoutStreams(t *testing.T) {
	f := newFixture(t)
	f.reconcile(t)
	if d := f.streamsDoc(t); len(d.Streams) != 0 || len(d.Pools) != 0 || d.Version == "" {
		t.Errorf("streams.json %+v, want an empty sealed document", d)
	}
}

func TestStreamsUnchangedNoWrite(t *testing.T) {
	f := newFixtureWith(t, streamSettings())
	f.site(t)
	f.newStream(t, "king5", "705", "http://192.168.0.58:5004/auto/v5.1")
	f.reconcile(t)
	before := f.streamsConfigMap(t)
	f.w.take()
	f.rec.take()
	f.reconcile(t)
	if ops := f.w.take(); len(ops) != 0 {
		t.Errorf("a second reconcile wrote %v", ops)
	}
	if after := f.streamsConfigMap(t); after.ResourceVersion != before.ResourceVersion {
		t.Errorf("%s changed (resourceVersion %s -> %s)", names.StreamsConfigMap, before.ResourceVersion, after.ResourceVersion)
	}
	if evs := f.rec.take(); len(evs) != 0 {
		t.Errorf("Events %+v", evs)
	}
}

// TestSourceChangeRewritesOnlyStreams: a new source (or a pool's new
// max) changes streams.json alone: <fullname>-streams is rewritten,
// pbx-config and pbx-secrets are not (no PBX reload), and no
// ConfigWritten goes out.
func TestSourceChangeRewritesOnlyStreams(t *testing.T) {
	f := newFixtureWith(t, streamSettings())
	f.site(t)
	f.newStream(t, "king5", "705", "http://192.168.0.58:5004/auto/v5.1")
	f.reconcile(t)
	bundleBefore, _ := f.compiled(t)
	docBefore := f.streamsDoc(t)
	cmBefore := f.configMap(t)
	f.w.take()
	f.rec.take()

	var st v1alpha1.Stream
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: "king5"}, &st); err != nil {
		t.Fatal(err)
	}
	st.Spec.Source = "http://192.168.0.58:5004/auto/v5.2"
	if err := f.c.Update(context.Background(), &st); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if ops := f.w.take(); !slices.Equal(ops, []string{"update ConfigMap/" + names.StreamsConfigMap}) {
		t.Fatalf("writes %v, want only %s", ops, names.StreamsConfigMap)
	}
	docAfter := f.streamsDoc(t)
	if docAfter.Version == docBefore.Version || docAfter.Streams[streams.UUID("king5")].Source != st.Spec.Source {
		t.Errorf("streams.json did not follow the source: %+v", docAfter)
	}
	bundleAfter, _ := f.compiled(t)
	if bundleAfter.Version != bundleBefore.Version || f.configMap(t).ResourceVersion != cmBefore.ResourceVersion {
		t.Error("a source change rewrote pbx-config")
	}
	if evs := f.rec.take(); len(evs) != 0 {
		t.Errorf("a streams-only change emitted %+v", evs)
	}
	if !strings.Contains(f.log.String(), "streams written") {
		t.Errorf("the streams write was not logged:\n%s", f.log)
	}
}

// TestEditedStreamsConfigMapRewritten: an edit of <fullname>-streams'
// data, version or labels, or its deletion, is written back.
func TestEditedStreamsConfigMapRewritten(t *testing.T) {
	f := newFixtureWith(t, streamSettings())
	f.newStream(t, "king5", "705", "http://192.168.0.58:5004/auto/v5.1")
	f.reconcile(t)
	good := f.streamsConfigMap(t)
	ctx := context.Background()
	edits := map[string]func(cm *corev1.ConfigMap){
		"the document": func(cm *corev1.ConfigMap) {
			cm.Data[install.StreamsKey] = strings.Replace(cm.Data[install.StreamsKey], "v5.1", "v9.1", 1)
		},
		"an added key":   func(cm *corev1.ConfigMap) { cm.Data["extra"] = "x" },
		"binary data":    func(cm *corev1.ConfigMap) { cm.BinaryData = map[string][]byte{"blob": {0xff}} },
		"the annotation": func(cm *corev1.ConfigMap) { cm.Annotations[install.AnnotationStreamsVersion] = strings.Repeat("0", 64) },
		"the labels":     func(cm *corev1.ConfigMap) { delete(cm.Labels, install.LabelManagedBy) },
	}
	for _, name := range slices.Sorted(maps.Keys(edits)) {
		cm := f.streamsConfigMap(t)
		edits[name](cm)
		if err := f.c.Update(ctx, cm); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.w.take()
		f.reconcile(t)
		now := f.streamsConfigMap(t)
		if !maps.Equal(now.Data, good.Data) || len(now.BinaryData) != 0 ||
			now.Annotations[install.AnnotationStreamsVersion] != good.Annotations[install.AnnotationStreamsVersion] ||
			now.Labels[install.LabelManagedBy] != install.ManagedBy {
			t.Errorf("an edit of %s was not written back", name)
		}
		if ops := f.w.take(); !slices.Equal(ops, []string{"update ConfigMap/" + names.StreamsConfigMap}) {
			t.Errorf("%s: writes %v, want only %s", name, ops, names.StreamsConfigMap)
		}
	}
	if err := f.c.Delete(ctx, good); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if now := f.streamsConfigMap(t); now.UID == good.UID || !maps.Equal(now.Data, good.Data) {
		t.Error("a deleted streams ConfigMap was not recreated with the same document")
	}
	f.streamsDoc(t)
}
