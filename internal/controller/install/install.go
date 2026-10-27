// Package install is the shared vocabulary of one patchbay install for the
// controller's components, the CLI and the e2e harness: the names of the
// objects the controller writes and reads, their keys, labels and
// annotations, how the compiled bundle is written to and read back from the
// cluster, how secrets are generated, and the Events the controller emits
// (docs/plans/release-a.md §6.2, §7.2).
//
// It is a contract: the four W3 build stages and the integrator build on it
// in parallel. Change it deliberately, and only with every user in view.
//
// Rules every user keeps:
//   - Object names come from Names (one fullname, as the chart computes it),
//     never from string literals.
//   - pbx-secrets is written before pbx-config, both built with
//     ProjectedSecret and ConfigMap, and only by the bundle reconciler.
//   - Compiled state is read back with ReadCompiled from the cache, on every
//     reconcile: nothing about the compiled bundle is kept in memory.
//   - Secrets are generated with this package's Generate functions from
//     crypto/rand (tests pass their own reader), never math/rand.
//   - Every object the controller creates carries ManagedLabels.
package install

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// MaxFullnameLength is the longest fullname the chart produces
// (patchbay.fullname truncates to 50), leaving room for every suffix.
const MaxFullnameLength = 50

// Names are the objects of one install, all in the PBX's namespace, derived
// from the chart's fullname (the helper patchbay.fullname: the release name,
// or <release>-patchbay).
type Names struct {
	// Fullname is the chart's fullname, e.g. "patchbay" or
	// "telephony-patchbay".
	Fullname string

	// ConfigMap is the compiled bundle, <fullname>-pbx-config: written by
	// the controller (ConfigMap), read by the agent and ReadCompiled.
	ConfigMap string
	// ProjectedSecret is <fullname>-pbx-secrets: every secret the bundle
	// references, under its compile.SecretRef.ProjectedKey, written by the
	// controller before ConfigMap (ProjectedSecret), read by the agent.
	ProjectedSecret string
	// AgentTokenSecret is <fullname>-agent-token, key AgentTokenKey: the
	// control API token, minted by the controller when missing (unowned),
	// mounted into the agent, read by the controller and the CLI.
	AgentTokenSecret string
	// AMISecret is <fullname>-ami, key AMIPasswordKey: the generated AMI
	// password (unowned). compile.Settings.AMISecret names it.
	AMISecret string
	// PushSecret is <fullname>-push, key PushPasswordKey: the generated
	// password the phones' push requests and the agent's authentication
	// URL share (unowned). compile.Settings.PushSecret names it.
	PushSecret string
	// Settings is <fullname>-settings, the chart's ConfigMap holding
	// SettingsKey: compile.Settings as JSON. The controller reads the file
	// it is mounted as; the object is only the target of CompileFailed
	// Events while ConfigMap does not exist yet.
	Settings string
	// AgentService is <fullname>-agent, the Service in front of the agent's
	// control API (AgentURL).
	AgentService string

	// StreamsConfigMap is <fullname>-streams (release B): streams.json
	// (StreamsKey), the compiled streams.Document, written by the bundle
	// reconciler before ConfigMap (StreamsConfigMap), mounted by the stream
	// bridge, read back with ReadStreams.
	StreamsConfigMap string
	// StreamBridge is <fullname>-streams too: the stream bridge's
	// Deployment, ServiceAccount and the Service in front of its
	// AudioSocket and control ports (StreamBridgeAddress, StreamBridgeURL).
	// Not <fullname>-stream-bridge: with a 50-character fullname that
	// Service name would be 64 characters, over the DNS label limit.
	StreamBridge string
}

// Keys of the install's Secrets and ConfigMaps.
const (
	// AgentTokenKey holds the control API token in AgentTokenSecret.
	AgentTokenKey = "token"
	// AMIPasswordKey holds the AMI password in AMISecret.
	AMIPasswordKey = "password"
	// PushPasswordKey holds the push password in PushSecret.
	PushPasswordKey = "password"
	// SettingsKey holds compile.Settings as JSON in the Settings ConfigMap,
	// and is the file name it is mounted as.
	SettingsKey = "settings.json"
	// StreamsKey holds streams.json in StreamsConfigMap, and is the file
	// name the bridge mounts it as (streams.FileName).
	StreamsKey = streams.FileName
)

// NewNames derives every name from the chart's fullname. Validate checks
// the result.
func NewNames(fullname string) Names {
	return Names{
		Fullname:         fullname,
		ConfigMap:        fullname + "-pbx-config",
		ProjectedSecret:  fullname + "-pbx-secrets",
		AgentTokenSecret: fullname + "-agent-token",
		AMISecret:        fullname + "-ami",
		PushSecret:       fullname + "-push",
		Settings:         fullname + "-settings",
		AgentService:     fullname + "-agent",
		StreamsConfigMap: fullname + "-streams",
		StreamBridge:     fullname + "-streams",
	}
}

// Validate checks that the fullname is a DNS label of at most
// MaxFullnameLength characters and that every name is a valid object name
// (the Service name a DNS-1035 label).
func (n Names) Validate() error {
	var errs []error
	if n.Fullname == "" || len(n.Fullname) > MaxFullnameLength {
		errs = append(errs, fmt.Errorf("install: fullname %q must be 1-%d characters", n.Fullname, MaxFullnameLength))
	}
	for _, p := range validation.IsDNS1123Label(n.Fullname) {
		errs = append(errs, fmt.Errorf("install: fullname %q: %s", n.Fullname, p))
	}
	for what, name := range map[string]string{
		"configMap": n.ConfigMap, "projectedSecret": n.ProjectedSecret, "agentTokenSecret": n.AgentTokenSecret,
		"amiSecret": n.AMISecret, "pushSecret": n.PushSecret, "settings": n.Settings, "streamsConfigMap": n.StreamsConfigMap,
	} {
		for _, p := range validation.IsDNS1123Subdomain(name) {
			errs = append(errs, fmt.Errorf("install: %s name %q: %s", what, name, p))
		}
	}
	for what, name := range map[string]string{"agent": n.AgentService, "stream bridge": n.StreamBridge} {
		for _, p := range validation.IsDNS1035Label(name) {
			errs = append(errs, fmt.Errorf("install: %s Service name %q: %s", what, name, p))
		}
	}
	return errors.Join(errs...)
}

// AMIRef is where the AMI password lives: what compile.Settings.AMISecret
// must be.
func (n Names) AMIRef() compile.SecretRef {
	return compile.SecretRef{Name: n.AMISecret, Key: AMIPasswordKey}
}

// PushRef is where the push password lives: what compile.Settings.PushSecret
// must be.
func (n Names) PushRef() compile.SecretRef {
	return compile.SecretRef{Name: n.PushSecret, Key: PushPasswordKey}
}

// AgentTokenRef is where the control API token lives.
func (n Names) AgentTokenRef() compile.SecretRef {
	return compile.SecretRef{Name: n.AgentTokenSecret, Key: AgentTokenKey}
}

// CheckSettings validates the settings and checks that they name this
// install's AMI Secret, so the chart (which renders settings.json) and the
// controller (which writes the Secret) agree on the fullname.
func CheckSettings(s compile.Settings, n Names) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.AMISecret != n.AMIRef() {
		return fmt.Errorf("settings: amiSecret is %s/%s, but this install's AMI Secret is %s/%s: the chart and the controller disagree on the fullname",
			s.AMISecret.Name, s.AMISecret.Key, n.AMISecret, AMIPasswordKey)
	}
	if s.PushSecret != n.PushRef() {
		return fmt.Errorf("settings: pushSecret is %s/%s, but this install's push Secret is %s/%s: the chart and the controller disagree on the fullname",
			s.PushSecret.Name, s.PushSecret.Key, n.PushSecret, PushPasswordKey)
	}
	return nil
}

// Labels and annotations on the objects the controller writes.
const (
	// LabelPartOf is the recommended part-of label; every patchbay object
	// has it with value PartOf (the chart's own objects too).
	LabelPartOf = "app.kubernetes.io/part-of"
	// PartOf is LabelPartOf's value.
	PartOf = "patchbay"
	// LabelManagedBy marks the objects the controller created (rather than
	// Helm): the compiled bundle, the projected Secret and every generated
	// Secret.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is LabelManagedBy's value on the controller's objects.
	ManagedBy = "patchbay-controller"

	// AnnotationBundleVersion is on ConfigMap and ProjectedSecret: the
	// bundle version they were written for. On the projected Secret it says
	// which version its values compile to, so a reader can tell a Secret
	// written ahead of its ConfigMap (ReadCompiled ignores its values until
	// the ConfigMap follows).
	AnnotationBundleVersion = "patchbay.chrismiller.xyz/bundle-version"
)

// LeaseSuffix ends the name of an install's leader-election Lease,
// <fullname>-controller.patchbay.chrismiller.xyz (controller.LeaderElectionID):
// patchbay status finds the installs of a namespace by it.
const LeaseSuffix = "-controller.patchbay.chrismiller.xyz"

// ManagedLabels are the labels every object the controller creates
// carries. Each call returns a new map.
func ManagedLabels() map[string]string {
	return map[string]string{LabelPartOf: PartOf, LabelManagedBy: ManagedBy}
}

// Controller timing (docs/plans/release-a.md §1.4, W3.1): controller-only
// values, not settings.
const (
	// PollInterval is how often the controller polls the agent's
	// observations.
	PollInterval = 5 * time.Second
	// StaleAfter is how old the last good poll may be before every device
	// is Unknown (drivers.DecideSettings.StaleAfter).
	StaleAfter = 30 * time.Second
)

// DecideSettings is what every driver's Decide gets from the install
// settings.
func DecideSettings(s compile.Settings) drivers.DecideSettings {
	return drivers.DecideSettings{Signalling: s.Signalling, StaleAfter: StaleAfter, PBXAddress: s.PBXAddress}
}

// DeviceHTTPURL is the base URL of the agent's device HTTP server,
// http://<pbx address>:<device HTTP port>: the same base Compile renders
// into device files (the 79x5's directoryURL).
func DeviceHTTPURL(s compile.Settings) string {
	return "http://" + s.PBXAddress + ":" + strconv.Itoa(s.DeviceHTTPPort)
}

// ProvisioningURL is a softphone's one-time provisioning URL for token:
// DeviceHTTPURL + api.ProvisioningPathPrefix + token. It is
// status.provisioningURL; the token in it must be the one the compiled
// bundle grants (Compiled.ProvisioningToken).
func ProvisioningURL(s compile.Settings, token string) string {
	return DeviceHTTPURL(s) + agentapi.ProvisioningPathPrefix + token
}

// AgentURL is the in-cluster base URL of the agent's control API, through
// its Service: http://<fullname>-agent.<namespace>.svc:<port>.
func AgentURL(n Names, namespace string, port int) string {
	return "http://" + n.AgentService + "." + namespace + ".svc:" + strconv.Itoa(port)
}

// StreamBridgeAddress is the stream bridge's AudioSocket address as
// Asterisk dials it, <fullname>-streams.<namespace>.svc:<port>: what the
// chart renders into settings.json's streamBridge (port 9092,
// streambridge/api.DefaultAudioSocketPort). The PBX pod resolves it
// through the cluster's DNS (dnsPolicy ClusterFirstWithHostNet).
func StreamBridgeAddress(n Names, namespace string, port int) string {
	return n.StreamBridge + "." + namespace + ".svc:" + strconv.Itoa(port)
}

// StreamBridgeURL is the in-cluster base URL of the stream bridge's
// control API (observations, metrics), through its Service:
// http://<fullname>-streams.<namespace>.svc:<port> (port 9093,
// streambridge/api.DefaultControlPort).
func StreamBridgeURL(n Names, namespace string, port int) string {
	return "http://" + StreamBridgeAddress(n, namespace, port)
}
