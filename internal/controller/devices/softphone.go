package devices

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
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
// (docs/plans/release-a.md §6.2.6, W3.5).
//
// status.provisioningURL carries the token the compiled bundle grants
// (6.2.1, "The provisioning URL shows the compiled token"): the URL for
// install.Compiled.ProvisioningToken when pbx-secrets matches pbx-config;
// "" when the softphone is not rendered (Invalid); otherwise, while a
// pbx-secrets written ahead of its pbx-config is not readable as compiled,
// the previous URL. A token rotated by this reconcile shows only once the
// bundle reconciler has compiled it.
func (r *SoftphoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s v1alpha1.Softphone
	if res, ok, err := r.read(ctx, req.NamespacedName, &s); !ok {
		return res, err
	}
	read := s.ResourceVersion
	dev := drivers.SoftphoneDevice(&s)
	v, ok, err := r.observe(ctx, dev.ID)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}
	d, now, ok := r.decide(&dev, v)
	if !ok {
		return ctrl.Result{}, nil
	}
	failed := r.act(ctx, &s, dev, d.Actions)

	before := s.Status.DeepCopy()
	after := s.Status.DeepCopy()
	after.State = string(d.State)
	after.ObservedGeneration = s.Generation
	setConditions(&after.Conditions, d.Conditions, s.Generation, now)
	memory(dev, d, failed).ToSoftphoneStatus(after)
	after.HumanAction = d.HumanAction
	if token, ok := v.compiled.ProvisioningToken(dev.ID); ok {
		after.ProvisioningURL = install.ProvisioningURL(r.cfg.Settings, token)
	} else if v.entry.ProvisioningToken == nil {
		after.ProvisioningURL = ""
	}

	if !equality.Semantic.DeepEqual(before, after) {
		s.Status = *after
		if err := r.cfg.Client.Status().Update(ctx, &s); err != nil {
			return ctrl.Result{}, statusWriteError("Softphone", s.Name, err)
		}
		r.writes.wrote(req.NamespacedName, read)
		r.written(&s, dev, d, v1alpha1.ActionRotateToken, v1alpha1.AnnotationReissueRequested, v.compiled.Index.Version,
			summary{before.State, before.HandledRequest, before.Conditions},
			summary{after.State, after.HandledRequest, after.Conditions})
	}
	return result(d, failed), nil
}
