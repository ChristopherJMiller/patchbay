package install_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// TestStreamBridgeNames: the bridge's Service fits a DNS label for the
// longest fullname, and Asterisk and the controller reach it by the names
// the chart renders.
func TestStreamBridgeNames(t *testing.T) {
	if got := install.StreamBridgeAddress(names, ns, 9092); got != "patchbay-streams.telephony.svc:9092" {
		t.Errorf("StreamBridgeAddress = %s", got)
	}
	if got := install.StreamBridgeURL(names, ns, 9093); got != "http://patchbay-streams.telephony.svc:9093" {
		t.Errorf("StreamBridgeURL = %s", got)
	}
	long := install.NewNames(strings.Repeat("a", install.MaxFullnameLength))
	if err := long.Validate(); err != nil || len(long.StreamBridge) > 63 {
		t.Errorf("a %d-character fullname: %v (Service %d characters)", install.MaxFullnameLength, err, len(long.StreamBridge))
	}
	bad := names
	bad.StreamBridge = "patchbay.streams"
	if err := bad.Validate(); err == nil {
		t.Error("a Service name with a dot validates")
	}
}

func sealedStreams(t *testing.T) streams.Document {
	t.Helper()
	d, err := streams.Document{
		Pools: []streams.Pool{{Name: "hdhomerun", Max: 1}},
		Streams: map[string]streams.Stream{
			streams.UUID("king5"): {Name: "king5", Number: "705", Source: "http://192.168.0.58:5004/auto/v5.1", Pool: "hdhomerun"},
		},
	}.Sealed()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestStreamsConfigMap: the builder writes the sealed document under
// streams.json with the managed labels and its version; ReadStreams reads
// it back, and says when it is not there yet.
func TestStreamsConfigMap(t *testing.T) {
	d := sealedStreams(t)
	cm, err := install.StreamsConfigMap(names, ns, d)
	if err != nil {
		t.Fatal(err)
	}
	if cm.Name != "patchbay-streams" || cm.Namespace != ns || cm.Annotations[install.AnnotationStreamsVersion] != d.Version ||
		cm.Labels[install.LabelManagedBy] != install.ManagedBy || len(cm.Data) != 1 || install.StreamsKey != "streams.json" {
		t.Errorf("ConfigMap: %+v", cm.ObjectMeta)
	}
	back, err := install.StreamsFromConfigMap(cm)
	if err != nil || back.Version != d.Version {
		t.Fatalf("StreamsFromConfigMap: %+v %v", back, err)
	}

	if _, err := install.StreamsConfigMap(names, ns, streams.Document{}); err == nil {
		t.Error("an unsealed document was written")
	}
	if _, err := install.StreamsFromConfigMap(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}); err == nil {
		t.Error("a ConfigMap without streams.json decoded")
	}

	ctx := context.Background()
	empty := fake.NewClientBuilder().Build()
	if _, err := install.ReadStreams(ctx, empty, ns, names); !errors.Is(err, install.ErrStreamsNotCompiled) {
		t.Errorf("ReadStreams with no ConfigMap: %v", err)
	}
	c := fake.NewClientBuilder().WithObjects(cm).Build()
	got, err := install.ReadStreams(ctx, c, ns, names)
	if err != nil || got.Streams[streams.UUID("king5")].Pool != "hdhomerun" {
		t.Errorf("ReadStreams: %+v %v", got, err)
	}
}
