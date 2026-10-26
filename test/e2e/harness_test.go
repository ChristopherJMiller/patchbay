//go:build e2e

// Package e2e is patchbay's end-to-end suite (design §6): a kind cluster, the
// real chart, the controller, real Asterisk 22 on the host network, the
// CLI, and fakephone, a simulated Cisco 79x5 and Linphone, running as
// ordinary Pods.
//
// Run it with `nix develop -c make e2e`; docs/e2e.md explains the knobs
// (PATCHBAY_E2E_REUSE, PATCHBAY_E2E_KEEP) and how to debug a failure.
//
// Every kind, kubectl and helm call carries --kubeconfig .e2e/kubeconfig, and
// KUBECONFIG is pointed there too, so nothing can fall back to
// ~/.kube/config or touch any cluster but patchbay-e2e.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

const (
	clusterName = "patchbay-e2e"
	namespace   = "patchbay-e2e"
	release     = "patchbay"

	patchbayImage = "ghcr.io/christopherjmiller/patchbay:e2e"
	toolsImage    = "ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e"

	// The release name contains the chart name, so the chart's fullname is
	// just "patchbay".
	controllerDeployment = release + "-controller"
	pbxDeployment        = release + "-pbx"
	agentService         = release + "-agent"
	agentTokenSecret     = release + "-agent-token"

	// The chart's default ports (test/e2e/values.yaml keeps them).
	sipPort        = 5060
	tftpPort       = 69
	ciscoHTTPPort  = 6970
	deviceHTTPPort = 8088
	controlPort    = 9440

	// fakephoneSelector labels every fakephone pod.
	fakephoneSelector = "app.kubernetes.io/name=fakephone"

	rolloutTimeout    = 5 * time.Minute
	kindCreateTimeout = 10 * time.Minute
)

var (
	testenv env.Environment
	h       *harness
)

// harness holds what the setup learned and the clients the tests share.
type harness struct {
	root       string    // repository root
	kubeconfig string    // <root>/.e2e/kubeconfig, the only kubeconfig used
	diagDir    string    // <root>/.e2e/diagnostics
	started    time.Time // when the suite started (a reused cluster's older pods are a previous run's)
	reuse      bool      // PATCHBAY_E2E_REUSE=1: keep an existing cluster
	keep       bool      // PATCHBAY_E2E_KEEP=1: do not delete the cluster at the end

	asteriskImage string            // the chart's Asterisk image, as loaded (tag, no digest)
	imageIDs      map[string]string // local image -> docker image ID

	nodeName string // the kind node (the PBX's pbx.nodeName)
	nodeIP   string // its InternalIP (the PBX's pbx.address)
	podCIDR  string // its pod CIDR, where fakephone runs (the allowed network)
	cs       *kubernetes.Clientset
	rest     *rest.Config
	// kube and kubeWatch know the patchbay types (the site resources,
	// status); kubeWatch also watches (tracker).
	kube      client.Client
	kubeWatch client.WithWatch

	cli      string           // <root>/.e2e/bin/patchbay, the e2e build of the CLI
	settings compile.Settings // what the chart rendered into <fullname>-settings
	// On a reused cluster, the PBX pod found at install and its
	// containers' restarts so far: a previous run's TestProvisioningOneTime
	// restarts the agent on purpose, and the pod survives an unchanged
	// chart. TestPBXUp allows those, and only those.
	priorPBX      types.UID
	priorRestarts map[string]int32
	agentToken    string // the control API token
	agentFwd      agentForward

	// stoppedDiagnosed is set once diagnostics were saved after the suite
	// was stopped (interrupt or budget); later failures are only fallout.
	stoppedDiagnosed atomic.Bool
}

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite sets the suite up, runs it and tears it down, and returns the exit
// code. It returns rather than exiting, so its deferred calls run.
func runSuite(m *testing.M) int {
	flag.Parse() // suiteBudget reads -test.timeout
	var err error
	h, err = newHarness()
	if err != nil {
		log.Printf("e2e: %v", err)
		return 1
	}
	// Belt and braces: any child process that ignores --kubeconfig still
	// lands on the e2e kubeconfig, never ~/.kube/config, and kubectl and
	// helm keep their discovery cache under .e2e too, not in ~/.kube/cache.
	for k, v := range h.childEnv() {
		if err := os.Setenv(k, v); err != nil {
			log.Printf("e2e: %v", err)
			return 1
		}
	}

	// Teardown is a Finish action, and only a returning testenv.Run runs it.
	// Two things would kill the process first and strand the cluster:
	//   - Ctrl-C or SIGTERM. Caught here, they cancel the suite's context
	//     instead: the running step fails fast and teardown still runs. A
	//     second signal kills as usual.
	//   - go test's -timeout alarm, which panics. The suite stops itself a
	//     margin earlier (suiteBudget), so teardown runs first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	if budget := suiteBudget(); budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
		log.Printf("suite budget %s (-test.timeout less a margin for teardown)", budget)
	}

	testenv, err = env.NewWithContext(ctx, envconf.NewWithKubeConfig(h.kubeconfig))
	if err != nil {
		log.Printf("e2e: %v", err)
		return 1
	}
	testenv.Setup(h.setupCluster, h.loadImages, h.install, h.waitForRollout, h.ready)
	testenv.AfterEachTest(h.diagnoseFailure)
	testenv.Finish(h.teardown)
	code := testenv.Run(m)
	if ctx.Err() != nil {
		log.Printf("e2e: stopped early: %v", context.Cause(ctx))
		code = max(code, 1)
	}
	return code
}

// suiteBudget is how long the whole suite (setup, tests, teardown) may run:
// -test.timeout less a margin, or 0 for no limit. go test's own timeout
// alarm panics without running teardown, so the suite must give up first.
// (The alarm only starts once the tests start, after setup, so a budget
// counted from TestMain always ends before it.)
func suiteBudget() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}
	getter, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	d, ok := getter.Get().(time.Duration)
	if !ok || d <= 0 {
		return 0
	}
	margin := min(max(d/10, 30*time.Second), 3*time.Minute, d/2)
	return d - margin
}

// childEnv is the environment every child process (kind, kubectl, helm)
// gets on top of the suite's: the e2e kubeconfig, and a kubectl/helm cache
// directory under .e2e instead of the shared ~/.kube/cache.
func (h *harness) childEnv() map[string]string {
	return map[string]string{
		"KUBECONFIG":   h.kubeconfig,
		"KUBECACHEDIR": filepath.Join(h.root, ".e2e", "kube-cache"),
	}
}

func newHarness() (*harness, error) {
	wd, err := os.Getwd() // go test runs in the package directory, test/e2e
	if err != nil {
		return nil, err
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "charts", "patchbay", "Chart.yaml")); err != nil {
		return nil, fmt.Errorf("cannot find the repository root from %s: %w", wd, err)
	}
	h := &harness{
		root:       root,
		started:    time.Now(),
		kubeconfig: filepath.Join(root, ".e2e", "kubeconfig"),
		diagDir:    filepath.Join(root, ".e2e", "diagnostics"),
		reuse:      os.Getenv("PATCHBAY_E2E_REUSE") == "1",
		keep:       os.Getenv("PATCHBAY_E2E_KEEP") == "1",
		imageIDs:   map[string]string{},
		cli:        filepath.Join(root, ".e2e", "bin", "patchbay"),
	}
	if _, err := os.Stat(h.cli); err != nil {
		return nil, fmt.Errorf("the e2e CLI is missing: run `nix develop -c make e2e-images` (make e2e does): %w", err)
	}
	h.asteriskImage, err = chartAsteriskImage(filepath.Join(root, "charts", "patchbay", "values.yaml"))
	if err != nil {
		return nil, err
	}
	return h, nil
}

// chartAsteriskImage reads repository:tag of the Asterisk image from the
// chart's values, so the image the suite loads is the one the chart pins.
func chartAsteriskImage(valuesFile string) (string, error) {
	raw, err := os.ReadFile(valuesFile)
	if err != nil {
		return "", err
	}
	var v struct {
		PBX struct {
			Asterisk struct {
				Image struct {
					Repository, Tag, Digest string
				} `json:"image"`
			} `json:"asterisk"`
		} `json:"pbx"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("%s: %w", valuesFile, err)
	}
	img := v.PBX.Asterisk.Image
	if img.Repository == "" || img.Tag == "" || img.Digest == "" {
		return "", fmt.Errorf("%s: pbx.asterisk.image needs repository, tag and digest", valuesFile)
	}
	return img.Repository + ":" + img.Tag, nil
}

// run runs a command from the repository root and returns its combined
// output. The command line is logged; the output only on failure. When ctx
// ends (an interrupt, the suite budget) the command gets SIGTERM, and
// SIGKILL 15 s later.
func (h *harness) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.root
	cmd.Env = os.Environ()
	for k, v := range h.childEnv() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	start := time.Now()
	out, err := cmd.CombinedOutput()
	log.Printf("$ %s %s  (%s)", name, strings.Join(args, " "), time.Since(start).Round(100*time.Millisecond))
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func (h *harness) kubectl(ctx context.Context, args ...string) (string, error) {
	return h.run(ctx, "kubectl", append([]string{"--kubeconfig", h.kubeconfig}, args...)...)
}

// setupCluster creates the kind cluster, or reuses it with
// PATCHBAY_E2E_REUSE=1. Without REUSE an existing cluster is deleted first,
// so a default run always starts from scratch.
func (h *harness) setupCluster(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	if err := os.MkdirAll(filepath.Dir(h.kubeconfig), 0o700); err != nil {
		return ctx, err
	}
	out, err := h.run(ctx, "kind", "get", "clusters")
	if err != nil {
		return ctx, err
	}
	exists := false
	for _, c := range strings.Fields(out) {
		exists = exists || c == clusterName
	}

	switch {
	case exists && h.reuse:
		log.Printf("reusing kind cluster %s", clusterName)
		// The kubeconfig may be gone; write it again, into .e2e only.
		if _, err := h.run(ctx, "kind", "export", "kubeconfig", "--name", clusterName, "--kubeconfig", h.kubeconfig); err != nil {
			return ctx, err
		}
	default:
		if exists {
			log.Printf("kind cluster %s exists; deleting it for a fresh run (PATCHBAY_E2E_REUSE=1 reuses it)", clusterName)
			if err := h.deleteCluster(ctx); err != nil {
				return ctx, err
			}
		}
		// --wait bounds only the wait for a Ready node; the timeout bounds
		// the whole create, a cold pull of the node image included.
		createCtx, cancel := context.WithTimeout(ctx, kindCreateTimeout)
		defer cancel()
		if _, err := h.run(createCtx, "kind", "create", "cluster", "--name", clusterName,
			"--config", filepath.Join("test", "e2e", "kind.yaml"),
			"--kubeconfig", h.kubeconfig, "--wait", "3m"); err != nil {
			return ctx, err
		}
	}

	h.rest = cfg.Client().RESTConfig()
	h.cs, err = kubernetes.NewForConfig(h.rest)
	if err != nil {
		return ctx, err
	}
	scheme, err := controller.NewScheme()
	if err != nil {
		return ctx, err
	}
	if h.kubeWatch, err = client.NewWithWatch(h.rest, client.Options{Scheme: scheme}); err != nil {
		return ctx, err
	}
	h.kube = h.kubeWatch
	nodes, err := h.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ctx, err
	}
	if len(nodes.Items) != 1 {
		return ctx, fmt.Errorf("want a one-node cluster, got %d nodes", len(nodes.Items))
	}
	node := nodes.Items[0]
	h.nodeName = node.Labels[corev1.LabelHostname]
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			h.nodeIP = a.Address
		}
	}
	h.podCIDR = node.Spec.PodCIDR
	if h.nodeName == "" || h.nodeIP == "" || h.podCIDR == "" {
		return ctx, fmt.Errorf("node %s has no hostname label, InternalIP or pod CIDR", node.Name)
	}
	log.Printf("node %s, InternalIP %s, pod CIDR %s", h.nodeName, h.nodeIP, h.podCIDR)

	// Docker starts every container, kind's node included, with
	// net.ipv4.ip_unprivileged_port_start=0, so in kind a non-root process
	// on the host network may bind port 69. A real node (Talos) keeps
	// Linux's 1024. Restore it, so the PBX pod's host ports behave here as
	// on the real node instead of hiding a missing capability.
	if _, err := h.run(ctx, "docker", "exec", h.nodeName,
		"sysctl", "-w", "net.ipv4.ip_unprivileged_port_start=1024"); err != nil {
		return ctx, fmt.Errorf("emulating the real node's privileged ports: %w", err)
	}
	return ctx, nil
}

// loadImages loads the locally built images (patchbay, the stream bridge,
// the tools), and the digest-pinned Asterisk image pulled on the host, into
// the kind node. `make e2e-images` builds and pulls them.
//
// Each image is saved for the host's platform only and loaded as an
// archive. `kind load docker-image` saves the whole image and imports
// every platform its index names (ctr --all-platforms); with Docker's
// containerd image store (the default on GitHub's ubuntu-26.04 runners)
// that index names platforms whose layers were never pulled, and the
// import fails with "content digest … not found".
func (h *harness) loadImages(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	dir, err := os.MkdirTemp("", "patchbay-e2e-images")
	if err != nil {
		return ctx, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	platform := "linux/" + runtime.GOARCH
	for i, img := range []string{patchbayImage, streamBridgeImage, toolsImage, h.asteriskImage} {
		id, err := h.run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", img)
		if err != nil {
			return ctx, fmt.Errorf("image %s is missing locally: run `nix develop -c make e2e-images` (make e2e does): %w", img, err)
		}
		h.imageIDs[img] = strings.TrimSpace(id)
		archive := filepath.Join(dir, fmt.Sprintf("image-%d.tar", i))
		if _, err := h.run(ctx, "docker", "save", "--platform", platform, "-o", archive, img); err != nil {
			return ctx, fmt.Errorf("saving %s for %s: %w", img, platform, err)
		}
		if _, err := h.run(ctx, "kind", "load", "image-archive", "--name", clusterName, archive); err != nil {
			return ctx, err
		}
		_ = os.Remove(archive)
	}
	return ctx, nil
}

// install creates the namespace and installs the chart with the e2e values
// plus what only runtime knows (node, address, the allowed network, image
// IDs), then the site resources (siteResources, with the site Streams)
// once the CRDs are served, and the fake stream sources (deployFakeSources).
// The controller then mints the install's Secrets and compiles the site,
// and the PBX pod and the stream bridge start on what it compiled
// (waitForRollout, ready).
func (h *harness) install(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespace,
		// What a real install needs for a hostNetwork pod (kind does not
		// enforce Pod Security, but the label documents it).
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "privileged"},
	}}
	if _, err := h.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctx, err
	}
	// A reused cluster starts without the previous run's fakephone pods (and
	// their scripts), so their logs never show up in this run's
	// diagnostics.
	if err := h.cs.CoreV1().Pods(namespace).DeleteCollection(ctx, metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: fakephoneSelector}); err != nil {
		return ctx, fmt.Errorf("deleting old fakephone pods: %w", err)
	}
	if err := h.cs.CoreV1().Secrets(namespace).DeleteCollection(ctx, metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: phonePodLabel}); err != nil {
		return ctx, fmt.Errorf("deleting old phone scripts: %w", err)
	}

	if h.reuse {
		if err := h.notePriorPBX(ctx); err != nil {
			return ctx, err
		}
	}

	runtimeValues := map[string]any{
		"controller": map[string]any{"podAnnotations": map[string]string{
			"e2e.patchbay.chrismiller.xyz/image-id": h.imageIDs[patchbayImage],
		}},
		"pbx": map[string]any{
			"nodeName": h.nodeName,
			"address":  h.nodeIP,
			"podAnnotations": map[string]string{
				"e2e.patchbay.chrismiller.xyz/image-id":          h.imageIDs[patchbayImage],
				"e2e.patchbay.chrismiller.xyz/asterisk-image-id": h.imageIDs[h.asteriskImage],
			},
		},
		"streamBridge": map[string]any{"podAnnotations": map[string]string{
			"e2e.patchbay.chrismiller.xyz/image-id": h.imageIDs[streamBridgeImage],
		}},
		// Only the pod network, where the phones run, may register: the
		// compiled ACL refuses every other source (TestRegister's
		// host-network REGISTER).
		"allowedNetworks": []string{h.podCIDR},
	}
	raw, err := yaml.Marshal(runtimeValues)
	if err != nil {
		return ctx, err
	}
	runtimeFile := filepath.Join(h.root, ".e2e", "values-runtime.yaml")
	if err := os.WriteFile(runtimeFile, raw, 0o600); err != nil {
		return ctx, err
	}
	if _, err = h.run(ctx, "helm", "upgrade", "--install", release, filepath.Join("charts", "patchbay"),
		"--kubeconfig", h.kubeconfig, "--namespace", namespace,
		"--values", filepath.Join("test", "e2e", "values.yaml"),
		"--values", runtimeFile); err != nil {
		return ctx, err
	}

	if err := h.waitForCRDs(ctx); err != nil {
		return ctx, err
	}
	for _, obj := range siteResources() {
		if err := h.applyResource(ctx, obj); err != nil {
			return ctx, fmt.Errorf("site resource %s: %w", obj.GetName(), err)
		}
	}
	log.Printf("site resources written: Phones %s and %s, Softphone %s, Person %s, Streams %s, %s, %s and %s",
		deskName, kitchenName, laptopName, personName, tunerAName, tunerBName, radioAName, radioBName)
	if err := deployFakeSources(ctx); err != nil {
		return ctx, fmt.Errorf("the fake stream sources: %w", err)
	}
	return ctx, nil
}

// ready reads what the tests share once both Deployments run: the settings
// the chart rendered, and the agent token the controller minted; then it
// waits until the agent has applied the controller's bundle.
func (h *harness) ready(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	cm, err := h.cs.CoreV1().ConfigMaps(namespace).Get(ctx, names.Settings, metav1.GetOptions{})
	if err != nil {
		return ctx, err
	}
	if h.settings, err = settings.Parse([]byte(cm.Data[install.SettingsKey])); err != nil {
		return ctx, fmt.Errorf("%s: %w", names.Settings, err)
	}
	tok, err := h.secretValue(ctx, names.AgentTokenRef())
	if err != nil {
		return ctx, fmt.Errorf("the agent token the controller mints: %w", err)
	}
	h.agentToken = strings.TrimSpace(string(tok))

	start := time.Now()
	var last string
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		b, err := h.readCompiled(ctx)
		if err != nil {
			last = err.Error()
			return false, nil //nolint:nilerr // not written yet: keep polling
		}
		obs, err := h.tryObservations(ctx)
		if err != nil {
			last = err.Error()
			return false, nil //nolint:nilerr // the agent is starting: keep polling
		}
		a := obs.Agent
		last = fmt.Sprintf("compiled %.12s; agent applied %.12s, last apply %.12s %q", b.Version, a.AppliedVersion, a.LastApplyVersion, a.LastApplyError)
		return a.AsteriskUp && a.AppliedVersion == b.Version, nil
	})
	if err != nil {
		path := h.saveDiagnostics(context.WithoutCancel(ctx), "setup-ready")
		return ctx, fmt.Errorf("the agent never applied the controller's bundle: %w (%s); diagnostics: %s", err, last, path)
	}
	log.Printf("the agent applied the controller's bundle (%s after the rollout)", time.Since(start).Round(time.Second))
	return ctx, nil
}

// notePriorPBX records the running PBX pod of a reused cluster, if any,
// and its containers' restart counts (harness.priorRestarts).
func (h *harness) notePriorPBX(ctx context.Context) error {
	list, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=pbx",
	})
	if err != nil {
		return fmt.Errorf("listing the PBX pod: %w", err)
	}
	for _, p := range list.Items {
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		h.priorPBX, h.priorRestarts = p.UID, map[string]int32{}
		for _, s := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			h.priorRestarts[s.Name] = s.RestartCount
		}
		log.Printf("reusing PBX pod %s; restarts so far %v", p.Name, h.priorRestarts)
	}
	return nil
}

// waitForRollout waits until the chart's Deployments (the controller, the
// PBX, the stream bridge) have fully rolled out, the way `kubectl rollout
// status` does, then the fake stream sources. On a timeout it saves
// diagnostics. The bridge waits for the controller's first compile (its
// <fullname>-streams volume is not optional), as the PBX pod waits for
// the agent token.
func (h *harness) waitForRollout(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	for _, name := range []string{controllerDeployment, pbxDeployment, names.StreamBridge} {
		start := time.Now()
		var last string
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, rolloutTimeout, true, func(ctx context.Context) (bool, error) {
			d, err := h.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			done, why := rolledOut(d)
			if why != last {
				log.Printf("deployment %s: %s", name, why)
				last = why
			}
			return done, nil
		})
		if err != nil {
			path := h.saveDiagnostics(context.WithoutCancel(ctx), "setup-"+name)
			if ctx.Err() != nil {
				return ctx, fmt.Errorf("deployment %s: stopped while waiting for its rollout (%s): %w; diagnostics: %s",
					name, last, context.Cause(ctx), path)
			}
			return ctx, fmt.Errorf("deployment %s did not roll out within %s (%s); diagnostics: %s", name, rolloutTimeout, last, path)
		}
		log.Printf("deployment %s rolled out in %s", name, time.Since(start).Round(time.Second))
	}
	start := time.Now()
	if err := fakeSourcesReady(ctx, rolloutTimeout); err != nil {
		path := h.saveDiagnostics(context.WithoutCancel(ctx), "setup-"+fakeSourcesName)
		return ctx, fmt.Errorf("%w; diagnostics: %s", err, path)
	}
	log.Printf("deployment %s rolled out in %s", fakeSourcesName, time.Since(start).Round(time.Second))
	return ctx, nil
}

// rolledOut is kubectl rollout status's test: the controller saw the latest
// spec and every replica is updated and available, with no old ones left.
func rolledOut(d *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	switch {
	case s.ObservedGeneration < d.Generation:
		return false, "waiting for the deployment controller to observe the new spec"
	case s.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d of %d replicas updated", s.UpdatedReplicas, want)
	case s.Replicas > s.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas still terminating", s.Replicas-s.UpdatedReplicas)
	case s.AvailableReplicas < s.UpdatedReplicas:
		return false, fmt.Sprintf("%d of %d updated replicas available", s.AvailableReplicas, s.UpdatedReplicas)
	}
	return true, "rolled out"
}

// diagnoseFailure saves (and logs the path of) cluster diagnostics after a
// failed test. Once the suite has been stopped, every remaining test fails
// fast; only the first of those gets diagnostics, so teardown is not held
// up.
func (h *harness) diagnoseFailure(ctx context.Context, _ *envconf.Config, t *testing.T) (context.Context, error) {
	if !t.Failed() {
		return ctx, nil
	}
	if ctx.Err() != nil && h.stoppedDiagnosed.Swap(true) {
		t.Logf("suite stopped (%v); no further diagnostics", context.Cause(ctx))
		return ctx, nil
	}
	t.Logf("diagnostics: %s", h.saveDiagnostics(context.WithoutCancel(ctx), t.Name()))
	return ctx, nil
}

// saveDiagnostics writes the state of the namespace (objects, events, pod
// descriptions, every container's logs) to a file under .e2e/diagnostics,
// prints it to the log, and returns the file's path.
func (h *harness) saveDiagnostics(ctx context.Context, label string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var b strings.Builder
	for _, args := range [][]string{
		{"get", "all,pvc,events", "--namespace", namespace, "-o", "wide"},
		{"get", "phones,softphones,people,streams,configmaps,secrets", "--namespace", namespace, "-o", "wide"},
		{"get", "phones,softphones,people,streams", "--namespace", namespace, "-o", "yaml"},
		{"get", "events.events.k8s.io", "--namespace", namespace, "--sort-by=.metadata.creationTimestamp"},
		{"describe", "pods", "--namespace", namespace},
		{"logs", "--namespace", namespace, "-l", "app.kubernetes.io/instance=" + release, "--all-containers", "--prefix", "--tail=300"},
		{"logs", "--namespace", namespace, "-l", "app.kubernetes.io/instance=" + release, "--all-containers", "--prefix", "--previous", "--tail=100"},
		{"logs", "--namespace", namespace, "-l", fakephoneSelector, "--prefix", "--tail=100"},
		{"logs", "--namespace", namespace, "-l", fakeSourcesLabel + "=" + fakeSourcesName, "--prefix", "--tail=100"},
		{"get", "--raw", "/api/v1/namespaces/" + namespace + "/services/http:" + names.StreamBridge + ":" + strconv.Itoa(bridgeapi.DefaultControlPort) + "/proxy" + bridgeapi.PathObservations},
		{"get", "events", "--all-namespaces", "--sort-by=.lastTimestamp"},
	} {
		out, err := h.kubectl(ctx, args...)
		fmt.Fprintf(&b, "\n===== kubectl %s\n%s", strings.Join(args, " "), out)
		if err != nil && out == "" {
			fmt.Fprintf(&b, "(error: %v)\n", err)
		}
	}
	name := strings.NewReplacer("/", "_", " ", "_").Replace(label)
	path := filepath.Join(h.diagDir, fmt.Sprintf("%s-%s.log", time.Now().Format("20060102-150405"), name))
	if err := os.MkdirAll(h.diagDir, 0o700); err == nil {
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			path = fmt.Sprintf("(not saved: %v)", err)
		}
	}
	log.Printf("diagnostics for %s:%s", label, b.String())
	return path
}

// teardown deletes the cluster unless PATCHBAY_E2E_KEEP=1.
func (h *harness) teardown(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	h.agentFwd.close()
	if h.keep {
		log.Printf("PATCHBAY_E2E_KEEP=1: keeping kind cluster %s (kubeconfig %s; `make e2e-down` deletes it)", clusterName, h.kubeconfig)
		return ctx, nil
	}
	if err := h.deleteCluster(context.WithoutCancel(ctx)); err != nil {
		log.Printf("teardown: %v", err)
		return ctx, err
	}
	return ctx, nil
}

func (h *harness) deleteCluster(ctx context.Context) error {
	_, err := h.run(ctx, "kind", "delete", "cluster", "--name", clusterName, "--kubeconfig", h.kubeconfig)
	return err
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
