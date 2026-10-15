package devices

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// SoftphoneReconciler decides Softphones (W3.5).
type SoftphoneReconciler struct {
	engine
}

// NewSoftphoneReconciler checks the config and builds the reconciler.
func NewSoftphoneReconciler(cfg Config) (*SoftphoneReconciler, error) {
	e, err := newEngine(cfg, softphoneController)
	if err != nil {
		return nil, err
	}
	return &SoftphoneReconciler{engine: e}, nil
}

// Reconcile decides one Softphone and writes its status when it changed
// (docs/plans/release-a.md §6.2.6, W3.5). status.provisioningURL is
// provisioningURL's.
func (r *SoftphoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s v1alpha1.Softphone
	if res, ok, err := r.read(ctx, req.NamespacedName, &s); !ok {
		return res, err
	}
	dev := drivers.SoftphoneDevice(&s)
	v, ok, err := r.observe(ctx, dev.ID)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}
	d, now, ok := r.decide(&dev, v)
	if !ok {
		return ctrl.Result{}, nil
	}
	url, err := r.provisioningURL(ctx, dev.ID, d, v, s.Status.ProvisioningURL)
	if err != nil {
		return ctrl.Result{}, err
	}
	acted := r.act(ctx, &s, dev, d.Actions)

	before := s.Status.DeepCopy()
	after := s.Status.DeepCopy()
	after.State = string(d.State)
	after.ObservedGeneration = s.Generation
	setConditions(&after.Conditions, conditions(d, acted), s.Generation, now)
	memory(dev, d, acted.failed()).ToSoftphoneStatus(after)
	after.HumanAction = humanAction(d)
	after.ProvisioningURL = url

	if !equality.Semantic.DeepEqual(before, after) {
		read, err := r.writeStatus(ctx, &s, func() { s.Status = *after }, acted.sent)
		if err != nil {
			return ctrl.Result{}, statusWriteError("Softphone", s.Name, err)
		}
		r.writes.wrote(req.NamespacedName, read)
		r.written(&s, dev, d, v1alpha1.ActionRotateToken, v1alpha1.AnnotationReissueRequested, v.compiled.Index.Version,
			summary{before.State, before.HandledRequest, before.Conditions},
			summary{after.State, after.HandledRequest, after.Conditions})
	}
	return result(d, acted.failed()), nil
}

// provisioningURL is what status.provisioningURL shows. The URL is a live
// grant: the agent serves the softphone's settings, SIP password included,
// once to whoever asks first. So it is published only while it is the
// one thing a person is asked to use (design §8.4: Issued), and only for
// the token the compiled bundle grants (6.2.1: pbx-secrets' value while
// its version is pbx-config's):
//   - "" unless the Decision's state is Issued (Provisioned, Registered,
//     Offline and Invalid show none; `qr --reissue` makes a new one), or
//     when the Decision replaces the token (a reissue's rotateToken);
//   - "" while the device Secret holds another token than the compiled one:
//     a rotation the bundle reconciler has not compiled yet, whose old
//     token the next bundle stops granting;
//   - the previous URL while pbx-secrets, written ahead of its pbx-config,
//     is not readable as compiled, unless the softphone is not rendered;
//   - otherwise the compiled token's URL.
func (r *SoftphoneReconciler) provisioningURL(ctx context.Context, id string, d drivers.Decision, v view, previous string) (string, error) {
	if d.State != drivers.StateIssued || holds(d.Actions, v1alpha1.ActionRotateToken) {
		return "", nil
	}
	token, ok := v.compiled.ProvisioningToken(id)
	switch {
	case !ok && v.entry.ProvisioningToken == nil:
		return "", nil
	case !ok:
		return previous, nil
	}
	name := compile.DeviceSecretName(id)
	var s corev1.Secret
	err := r.cfg.Client.Get(ctx, client.ObjectKey{Namespace: r.cfg.Namespace, Name: name}, &s)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("devices: reading Secret %s: %w", name, err)
	}
	if string(s.Data[compile.SecretKeyProvisioningToken]) != token {
		r.log.V(1).Info("no provisioning URL: the device Secret's token is not compiled yet", "device", id)
		return "", nil
	}
	return install.ProvisioningURL(r.cfg.Settings, token), nil
}
