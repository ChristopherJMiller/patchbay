package devices

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// PhoneReconciler decides Phones (W3.5).
type PhoneReconciler struct {
	engine
}

// NewPhoneReconciler checks the config and builds the reconciler.
func NewPhoneReconciler(cfg Config) (*PhoneReconciler, error) {
	e, err := newEngine(cfg, phoneController)
	if err != nil {
		return nil, err
	}
	return &PhoneReconciler{engine: e}, nil
}

// Reconcile decides one Phone and writes its status when it changed
// (docs/plans/release-a.md §6.2.6, W3.5).
func (r *PhoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Phone
	if res, ok, err := r.read(ctx, req.NamespacedName, &p); !ok {
		return res, err
	}
	read := p.ResourceVersion
	dev := drivers.PhoneDevice(&p)
	v, ok, err := r.observe(ctx, dev.ID)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}
	d, now, ok := r.decide(&dev, v)
	if !ok {
		return ctrl.Result{}, nil
	}
	failed := r.act(ctx, &p, dev, d.Actions)

	before := p.Status.DeepCopy()
	after := p.Status.DeepCopy()
	after.State = string(d.State)
	after.ObservedGeneration = p.Generation
	setConditions(&after.Conditions, d.Conditions, p.Generation, now)
	memory(dev, d, failed).ToPhoneStatus(after)
	after.HumanAction = humanAction(d)

	if !equality.Semantic.DeepEqual(before, after) {
		p.Status = *after
		if err := r.cfg.Client.Status().Update(ctx, &p); err != nil {
			return ctrl.Result{}, statusWriteError("Phone", p.Name, err)
		}
		r.writes.wrote(req.NamespacedName, read)
		r.written(&p, dev, d, v1alpha1.ActionResync, v1alpha1.AnnotationResyncRequested, v.compiled.Index.Version,
			summary{before.State, before.HandledRequest, before.Conditions},
			summary{after.State, after.HandledRequest, after.Conditions})
	}
	return result(d, failed), nil
}
