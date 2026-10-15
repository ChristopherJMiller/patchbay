package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// LeaseStale is how long since its last renewal status calls an install's
// leader-election Lease stale: controller-runtime renews it every 2 s and
// gives it up after 15 s.
const LeaseStale = 30 * time.Second

// controllerLines are status' lines about each install's controller in
// the namespace: who holds its Lease and when it renewed it; the version
// in pbx-config beside the one the agent applied (applied, "" when the
// agent could not be asked); and the latest CompileFailed Event when it is
// newer than the latest ConfigWritten (a compile failure is otherwise only
// an Event, a metric and a log line). A read that fails is a line saying
// so: this is part of an overview, never a reason to fail it.
func controllerLines(ctx context.Context, env *Env, applied string) []string {
	var leases coordinationv1.LeaseList
	if err := env.Kube.List(ctx, &leases, client.InNamespace(env.Namespace)); err != nil {
		return []string{"Controller: could not read its Lease: " + err.Error()}
	}
	var lines []string
	for _, l := range leases.Items {
		fullname, ok := strings.CutSuffix(l.Name, install.LeaseSuffix)
		if !ok {
			continue
		}
		lines = append(lines, leaseLine(fullname, l, env.Clock.Now()))
		lines = append(lines, configLines(ctx, env, install.NewNames(fullname), applied)...)
	}
	if lines == nil {
		return []string{fmt.Sprintf("Controller: no controller Lease in namespace %s: none has run here", env.Namespace)}
	}
	return lines
}

func leaseLine(fullname string, l coordinationv1.Lease, now time.Time) string {
	holder := ""
	if l.Spec.HolderIdentity != nil {
		holder = *l.Spec.HolderIdentity
	}
	if holder == "" || l.Spec.RenewTime == nil {
		return "Controller " + fullname + ": nobody holds the lease: nothing decides or compiles, and what follows is as last written"
	}
	age := now.Sub(l.Spec.RenewTime.Time).Round(time.Second)
	if age > LeaseStale {
		return fmt.Sprintf("Controller %s: %s last renewed its lease %s ago: it may be down, and what follows is as it last wrote it",
			fullname, holder, age)
	}
	return fmt.Sprintf("Controller %s: %s, lease renewed %s ago", fullname, holder, max(age, 0))
}

func configLines(ctx context.Context, env *Env, names install.Names, applied string) []string {
	var cm corev1.ConfigMap
	err := env.Kube.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: names.ConfigMap}, &cm)
	var lines []string
	switch {
	case apierrors.IsNotFound(err):
		lines = append(lines, "Config: "+names.ConfigMap+" does not exist: nothing compiled yet")
	case err != nil:
		lines = append(lines, "Config: could not read "+names.ConfigMap+": "+err.Error())
	default:
		compiled := cm.Annotations[install.AnnotationBundleVersion]
		switch applied {
		case "":
			lines = append(lines, "Config: version "+shortVersion(compiled)+" compiled")
		case compiled:
			lines = append(lines, "Config: version "+shortVersion(compiled)+" compiled and applied")
		default:
			lines = append(lines, fmt.Sprintf("Config: version %s compiled, the agent has %s applied", shortVersion(compiled), shortVersion(applied)))
		}
	}
	if line, ok := compileFailure(ctx, env, names); ok {
		lines = append(lines, line)
	}
	return lines
}

// compileFailure is the line for the latest CompileFailed Event when it is
// newer than the latest ConfigWritten.
func compileFailure(ctx context.Context, env *Env, names install.Names) (string, bool) {
	var events eventsv1.EventList
	if err := env.Kube.List(ctx, &events, client.InNamespace(env.Namespace)); err != nil {
		return "Config: could not read its Events: " + err.Error(), true
	}
	var failed *eventsv1.Event
	var written time.Time
	for i := range events.Items {
		e := &events.Items[i]
		if e.Regarding.Kind != "ConfigMap" || !slices.Contains([]string{names.ConfigMap, names.Settings}, e.Regarding.Name) {
			continue
		}
		switch e.Reason {
		case install.EventReasonConfigWritten:
			if at := eventTime(e); at.After(written) {
				written = at
			}
		case install.EventReasonCompileFailed:
			if failed == nil || eventTime(e).After(eventTime(failed)) {
				failed = e
			}
		}
	}
	if failed == nil || !eventTime(failed).After(written) {
		return "", false
	}
	return fmt.Sprintf("Config: compiling has failed since %s, the version above stays: %s",
		eventTime(failed).UTC().Format(time.RFC3339), failed.Note), true
}

// eventTime is when an Event (or the last of its series) happened.
func eventTime(e *eventsv1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}
