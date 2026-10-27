package chart_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/ChristopherJMiller/patchbay/internal/controller"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
)

// tokenSecret is the token Secret the messages tests name.
const tokenSecret = "patchbay-messages-token"

// messagesPort is the controller container's port named messages, 0 for
// none.
func messagesPort(c *corev1.Container) int32 {
	for _, p := range c.Ports {
		if p.Name == "messages" {
			return p.ContainerPort
		}
	}
	return 0
}

// TestChartMessagesService: with messages.enabled the controller serves
// the messages endpoint (internal/controller/messages, every replica) on a
// container port named messages, never metrics or control (the PodMonitor
// scrapes those), at --messages-bind-address=:<port> with
// --messages-token-secret=<name>/<key> as controller.ParseMessagesToken
// reads it; the token is read through the API on every request, so nothing
// is mounted. The Service <fullname>-messages (install.Names.MessagesService)
// is ClusterIP only, port messages to the controller pods. The port defaults
// to messagesapi.DefaultPort and moves with messages.port, the key with
// tokenSecret.key.
func TestChartMessagesService(t *testing.T) {
	for _, tc := range []struct {
		name string
		sets []string
		port int
		key  string
	}{
		{"defaults", []string{"messages.enabled=true", "messages.tokenSecret.name=" + tokenSecret}, messagesapi.DefaultPort, "token"},
		{"port and key", []string{"messages.enabled=true", "messages.tokenSecret.name=" + tokenSecret,
			"messages.port=9541", "messages.tokenSecret.key=bearer"}, 9541, "bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := render(t, tc.sets...)
			c := controllerContainer(t, m)
			wantFlags(t, c, map[string]string{
				"messages-bind-address": ":" + strconv.Itoa(tc.port),
				"messages-token-secret": tokenSecret + "/" + tc.key,
			})
			arg, _ := flag(c.Args, "messages-token-secret")
			if name, key, err := controller.ParseMessagesToken(arg); err != nil || name != tokenSecret || key != tc.key {
				t.Errorf("the controller reads --messages-token-secret=%s as %q %q (%v)", arg, name, key, err)
			}
			if got := messagesPort(c); got != int32(tc.port) {
				t.Errorf("the controller's messages port is %d, want %d (ports %+v)", got, tc.port, c.Ports)
			}
			if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != "settings" {
				t.Errorf("the controller mounts %+v with messages on: the token is read through the API, not mounted", c.VolumeMounts)
			}
			for _, e := range c.Env {
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					t.Errorf("the controller has the Secret %s in its environment", e.ValueFrom.SecretKeyRef.Name)
				}
			}

			svc := get[corev1.Service](t, m, "Service", names.MessagesService)
			if svc.Spec.Type != corev1.ServiceTypeClusterIP {
				t.Errorf("the messages Service is %q, want ClusterIP (nothing new on the LAN)", svc.Spec.Type)
			}
			if len(svc.Spec.ExternalIPs) != 0 || svc.Spec.LoadBalancerIP != "" {
				t.Errorf("the messages Service has external addresses: %+v", svc.Spec)
			}
			got := map[string]string{}
			for _, p := range svc.Spec.Ports {
				got[p.Name] = fmt.Sprintf("%d->%s/%s", p.Port, p.TargetPort.String(), p.Protocol)
			}
			if want := map[string]string{"messages": fmt.Sprintf("%d->messages/TCP", tc.port)}; !reflect.DeepEqual(got, want) {
				t.Errorf("Service ports %v, want %v", got, want)
			}
			pod := get[appsv1.Deployment](t, m, "Deployment", release+"-controller").Spec.Template
			for k, v := range svc.Spec.Selector {
				if pod.Labels[k] != v {
					t.Errorf("the Service selects %s=%s, which the controller pod lacks", k, v)
				}
			}
			if svc.Spec.Selector["app.kubernetes.io/component"] != "controller" || svc.Spec.Selector["app.kubernetes.io/instance"] != release {
				t.Errorf("the Service selects %v: not only this install's controller", svc.Spec.Selector)
			}
		})
	}

	// The PodMonitor still scrapes only metrics and control.
	pm := podMonitor(t, render(t, "messages.enabled=true", "messages.tokenSecret.name="+tokenSecret, "podMonitor.enabled=true"))
	for _, e := range pm.Spec.Endpoints {
		if e.Port == "messages" {
			t.Error("the PodMonitor scrapes the messages port")
		}
	}

	// Off (the default): no Service, no flags, no port.
	for _, m := range []manifest{render(t), renderFile(t, filepath.Join(chartDir, "ci", "no-streams-values.yaml"))} {
		if _, ok := m.docs["Service/"+names.MessagesService]; ok {
			t.Errorf("a Service %s is rendered with messages off", names.MessagesService)
		}
		c := controllerContainer(t, m)
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--messages-") {
				t.Errorf("the controller gets %s with messages off", a)
			}
		}
		if p := messagesPort(c); p != 0 {
			t.Errorf("the controller has a messages port %d with messages off", p)
		}
	}

	// The all-features values turn it on with a port of their own.
	all := renderFile(t, filepath.Join(chartDir, "ci", "all-features-values.yaml"))
	wantFlags(t, controllerContainer(t, all), map[string]string{"messages-bind-address": ":9541"})
	if svc := get[corev1.Service](t, all, "Service", names.MessagesService); len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 9541 {
		t.Errorf("the all-features messages Service has ports %+v, want 9541", svc.Spec.Ports)
	}
}

// TestChartMessagesNeedsTheToken: messages.enabled without a token Secret
// name, or with an empty key, fails `helm template`, naming the value.
func TestChartMessagesNeedsTheToken(t *testing.T) {
	minimal := filepath.Join(chartDir, "ci", "minimal-values.yaml")
	for _, tc := range []struct {
		sets []string
		want string
	}{
		{[]string{"messages.enabled=true"}, "messages.tokenSecret.name is required"},
		{[]string{"messages.enabled=true", "messages.tokenSecret.name=" + tokenSecret, "messages.tokenSecret.key="}, "messages.tokenSecret.key is required"},
	} {
		_, stderr, err := helmTemplate(t, minimal, tc.sets...)
		if err == nil || !strings.Contains(stderr, tc.want) {
			t.Errorf("--set %v: err %v, stderr %q; want a failure naming %q", tc.sets, err, stderr, tc.want)
		}
	}
}

// TestChartNotesMessages: NOTES name the messages endpoint's URL and its
// token Secret when it is on, and <fullname>-house among the ConfigMaps the
// controller leaves on uninstall either way (the endpoint may have written
// it while it was on).
func TestChartNotesMessages(t *testing.T) {
	on := installNotes(t, "messages.enabled=true", "messages.tokenSecret.name="+tokenSecret)
	for _, want := range []string{
		fmt.Sprintf("http://%s.telephony.svc:%d%s", names.MessagesService, messagesapi.DefaultPort, messagesapi.PathMessages),
		messagesapi.PathHouse,
		tokenSecret,
		names.HouseConfigMap,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("NOTES lack %q:\n%s", want, on)
		}
	}
	off := installNotes(t)
	if strings.Contains(off, names.MessagesService) || !strings.Contains(off, names.HouseConfigMap) ||
		!slices.ContainsFunc(strings.Split(off, "\n"), func(l string) bool {
			return strings.Contains(l, "delete configmap") && strings.Contains(l, names.HouseConfigMap)
		}) {
		t.Errorf("NOTES with messages off:\n%s", off)
	}
}
