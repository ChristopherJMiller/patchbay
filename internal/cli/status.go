package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// statusAgentTimeout bounds how long status waits for the agent (the
// port-forward and the observations): it is one line of an overview, not
// a reason to hang.
const statusAgentTimeout = 30 * time.Second

// healthyStatus is the status each known condition type has when nothing
// is wrong. Status prints every condition not in it; a type not listed
// here (a newer controller's) is always printed.
var healthyStatus = map[string]metav1.ConditionStatus{
	v1alpha1.ConditionReady:             metav1.ConditionTrue,
	v1alpha1.ConditionFirmwareAvailable: metav1.ConditionTrue,
	v1alpha1.ConditionFirmwareCurrent:   metav1.ConditionTrue,
	v1alpha1.ConditionProvisioned:       metav1.ConditionTrue,
	v1alpha1.ConditionRegistered:        metav1.ConditionTrue,
	v1alpha1.ConditionConfigCurrent:     metav1.ConditionTrue,
	v1alpha1.ConditionNumberConflict:    metav1.ConditionFalse,
	v1alpha1.ConditionMACConflict:       metav1.ConditionFalse,
	v1alpha1.ConditionUnsupported:       metav1.ConditionFalse,
	v1alpha1.ConditionLabelShortened:    metav1.ConditionFalse,
}

// resourceRow is one resource as status prints it.
type resourceRow struct {
	kind, name, number, state string
	// generation and observed are metadata.generation and
	// status.observedGeneration: a resource whose spec changed since the
	// controller last decided it says so.
	generation, observed int64
	conditions           []metav1.Condition
	humanAction          string
	// provisioning is a Softphone with a provisioning URL. The URL itself
	// is never printed: only `patchbay softphone qr` prints it.
	provisioning bool
}

// Status prints an overview of the install (plan 6.2.7):
//
//  1. the agent: its version, whether Asterisk is up, the applied config
//     version and a failed last apply; then, per install in the namespace
//     (one leader-election Lease each), the controller: who holds the
//     Lease and when it renewed it, the version it last compiled beside
//     the one the agent applied, and a compile failure newer than the
//     last version written (controllerLines). Without them a stopped
//     controller or a frozen compile looks like a healthy install;
//  2. per kind (Phone, Softphone, Person, Stream; by name; a kind with none
//     left out) each resource's state (Pending before the controller wrote
//     one; marked when its spec changed since), every condition not in its healthy status, its human action, and
//     for a Softphone with a URL the command that shows it;
//  3. the unknown phones the agent has seen ask for config.
//
// The resources are listed first; failing that is the only error. The
// agent is reached through env.Agent (GET /v1/observations); when it
// cannot be, lines 1 and 3 say so and Status still succeeds.
func Status(ctx context.Context, env *Env) error {
	sections, err := listResources(ctx, env.Kube, env.Namespace)
	if err != nil {
		return err
	}
	agentCtx, cancel := context.WithTimeout(ctx, statusAgentTimeout)
	doc, agentErr := observations(agentCtx, env.Agent)
	cancel()

	p := &printer{w: env.Out}
	if agentErr != nil {
		p.printf("Agent: could not ask the agent: %s\n", printable(agentErr.Error()))
	} else {
		p.printf("Agent: %s\n", printable(agentSummary(doc.Agent)))
	}
	applied := ""
	if agentErr == nil {
		applied = doc.Agent.AppliedVersion
	}
	for _, line := range controllerLines(ctx, env, applied) {
		p.printf("%s\n", printable(line))
	}
	for _, rows := range sections {
		p.printf("\n")
		for _, r := range rows {
			printRow(p, r)
		}
	}
	p.printf("\n")
	switch {
	case agentErr != nil:
		p.printf("Unknown phones: could not ask the agent: %s\n", printable(agentErr.Error()))
	case len(doc.UnknownDevices) == 0:
		p.printf("Unknown phones: none\n")
	default:
		p.printf("Unknown phones:\n")
		unknown := slices.Clone(doc.UnknownDevices)
		slices.SortFunc(unknown, func(a, b agentapi.UnknownDevice) int { return cmp.Compare(a.MAC, b.MAC) })
		for _, u := range unknown {
			p.printf("  %s from %s, first seen %s, last seen %s, asked for %s\n",
				printable(u.MAC), printable(u.Source), u.FirstSeen.UTC().Format(time.RFC3339), u.LastSeen.UTC().Format(time.RFC3339), printable(u.File))
		}
	}
	return p.err
}

// listResources lists every kind in the namespace: one section per kind
// that has any, rows by name.
func listResources(ctx context.Context, kube client.Reader, namespace string) ([][]resourceRow, error) {
	in := client.InNamespace(namespace)
	var phones v1alpha1.PhoneList
	var softphones v1alpha1.SoftphoneList
	var people v1alpha1.PersonList
	var streams v1alpha1.StreamList
	for _, l := range []struct {
		kind string
		list client.ObjectList
	}{{"Phones", &phones}, {"Softphones", &softphones}, {"People", &people}, {"Streams", &streams}} {
		if err := kube.List(ctx, l.list, in); err != nil {
			return nil, fmt.Errorf("listing %s in namespace %s: %w", l.kind, namespace, err)
		}
	}
	var sections [][]resourceRow
	add := func(rows []resourceRow) {
		if len(rows) == 0 {
			return
		}
		slices.SortFunc(rows, func(a, b resourceRow) int { return cmp.Compare(a.name, b.name) })
		sections = append(sections, rows)
	}
	var rows []resourceRow
	for _, o := range phones.Items {
		rows = append(rows, resourceRow{
			kind: "Phone", name: o.Name, number: string(o.Spec.Number), state: o.Status.State,
			generation: o.Generation, observed: o.Status.ObservedGeneration,
			conditions: o.Status.Conditions, humanAction: o.Status.HumanAction,
		})
	}
	add(rows)
	rows = nil
	for _, o := range softphones.Items {
		rows = append(rows, resourceRow{
			kind: "Softphone", name: o.Name, number: string(o.Spec.Number), state: o.Status.State,
			generation: o.Generation, observed: o.Status.ObservedGeneration,
			conditions: o.Status.Conditions, humanAction: o.Status.HumanAction,
			provisioning: o.Status.ProvisioningURL != "",
		})
	}
	add(rows)
	rows = nil
	for _, o := range people.Items {
		rows = append(rows, resourceRow{
			kind: "Person", name: o.Name, number: string(o.Spec.Number), state: o.Status.State,
			generation: o.Generation, observed: o.Status.ObservedGeneration,
			conditions: o.Status.Conditions,
		})
	}
	add(rows)
	rows = nil
	for _, o := range streams.Items {
		rows = append(rows, resourceRow{
			kind: "Stream", name: o.Name, number: string(o.Spec.Number), state: o.Status.State,
			generation: o.Generation, observed: o.Status.ObservedGeneration,
			conditions: o.Status.Conditions,
		})
	}
	add(rows)
	return sections, nil
}

func printRow(p *printer, r resourceRow) {
	state := displayState(r.state)
	if r.state != "" && r.observed < r.generation {
		state += " (its spec changed since; not decided again yet)"
	}
	p.printf("%s %s (%s): %s\n", r.kind, r.name, printable(r.number), printable(state))
	for _, c := range r.conditions {
		if want, known := healthyStatus[c.Type]; known && c.Status == want {
			continue
		}
		line := fmt.Sprintf("%s=%s %s", c.Type, c.Status, c.Reason)
		if c.Message != "" {
			line += ": " + c.Message
		}
		p.printf("  %s\n", indentLines(printable(line)))
	}
	if r.humanAction != "" {
		p.printf("  human action: %s\n", indentLines(printable(r.humanAction)))
	}
	if r.provisioning {
		p.printf("  provisioning: patchbay softphone qr %s\n", r.name)
	}
}

// displayState is status.state, or Pending before the controller wrote one.
func displayState(state string) string {
	if state == "" {
		return "Pending"
	}
	return state
}

// printable is text from the cluster or the agent as status prints it:
// valid UTF-8, each control character but a newline a space. Status text
// can carry what a device sent (its web page, its SIP user agent), and a
// terminal escape or carriage return in it could rewrite the lines above.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}

// indentLines keeps a multi-line message under its row.
func indentLines(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

// agentSummary is the agent line after "Agent: ".
func agentSummary(a agentapi.AgentStatus) string {
	version := a.Version
	if version == "" {
		version = "unknown version"
	}
	s := version + ", "
	switch {
	case !a.AsteriskUp:
		s += "Asterisk down"
	case a.AsteriskVersion == "":
		s += "Asterisk up"
	default:
		s += "Asterisk up (" + a.AsteriskVersion + ")"
	}
	if a.AppliedVersion == "" {
		s += ", no config applied yet"
	} else {
		s += ", config " + shortVersion(a.AppliedVersion) + " applied"
	}
	if a.LastApplyError != "" {
		s += ", last apply of " + shortVersion(a.LastApplyVersion) + " failed: " + a.LastApplyError
	}
	return s
}

// shortVersion is the first 12 characters of a bundle version, as git
// shortens a hash.
func shortVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// printer writes formatted output and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}
