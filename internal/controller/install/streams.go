package install

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// The compiled streams in the cluster (release B, docs/plans/release-b.md
// §7.2): the bundle reconciler writes StreamsConfigMap, <fullname>-streams,
// holding compile.Bundle.Streams Encoded under StreamsKey, before it writes
// pbx-secrets and pbx-config, and only when its version changed. So a new
// Stream is in the file the bridge mounts before Asterisk's dialplan routes
// its number (the kubelet still takes up to about a minute to update the
// mounted file; the Stream is BridgePending meanwhile). The stream bridge
// reads only the mounted file; the controller reads the object back with
// ReadStreams, from the cache.

// AnnotationStreamsVersion is on StreamsConfigMap: the version of the
// streams.Document it holds.
const AnnotationStreamsVersion = "patchbay.chrismiller.xyz/streams-version"

// ErrStreamsNotCompiled is ReadStreams' error while StreamsConfigMap does
// not exist yet. A Stream reconciler writes no status then; the
// ConfigMap's creation enqueues every Stream.
var ErrStreamsNotCompiled = errors.New("install: the streams have not been compiled yet (no <fullname>-streams ConfigMap)")

// StreamsConfigMap builds <fullname>-streams for d, which must be sealed
// (Encode Validates it): StreamsKey -> d.Encode(), ManagedLabels and
// AnnotationStreamsVersion.
func StreamsConfigMap(n Names, namespace string, d streams.Document) (*corev1.ConfigMap, error) {
	raw, err := d.Encode()
	if err != nil {
		return nil, fmt.Errorf("install: %s: %w", n.StreamsConfigMap, err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        n.StreamsConfigMap,
			Namespace:   namespace,
			Labels:      ManagedLabels(),
			Annotations: map[string]string{AnnotationStreamsVersion: d.Version},
		},
		Data: map[string]string{StreamsKey: string(raw)},
	}, nil
}

// StreamsFromConfigMap decodes a <fullname>-streams ConfigMap
// (streams.Decode: strict, validated).
func StreamsFromConfigMap(cm *corev1.ConfigMap) (streams.Document, error) {
	raw, ok := cm.Data[StreamsKey]
	if !ok {
		return streams.Document{}, fmt.Errorf("install: ConfigMap %s has no %s", cm.Name, StreamsKey)
	}
	d, err := streams.Decode([]byte(raw))
	if err != nil {
		return streams.Document{}, fmt.Errorf("install: ConfigMap %s: %w", cm.Name, err)
	}
	return d, nil
}

// ReadStreams reads StreamsConfigMap through c (the manager's cached
// client) and decodes it. It returns ErrStreamsNotCompiled when the
// ConfigMap does not exist.
func ReadStreams(ctx context.Context, c client.Reader, namespace string, n Names) (streams.Document, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: n.StreamsConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return streams.Document{}, ErrStreamsNotCompiled
		}
		return streams.Document{}, fmt.Errorf("install: reading ConfigMap %s: %w", n.StreamsConfigMap, err)
	}
	return StreamsFromConfigMap(&cm)
}
