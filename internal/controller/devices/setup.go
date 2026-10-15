package devices

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// Controller names (6.2.6).
const (
	phoneController     = "phone"
	softphoneController = "softphone"
)

// Setup registers both reconcilers with the manager (6.2.6, "Watches"):
//   - the resource itself, for a new generation (spec) or annotation (a
//     request), so the controller's own status writes never enqueue it;
//   - the pbx-config ConfigMap: a new bundle enqueues every device of the
//     kind;
//   - the poller's Changes channel of the kind;
//   - for Softphones also the pbx-secrets Secret, which the provisioning
//     URL is read from: its informer can deliver it after pbx-config's.
func Setup(mgr ctrl.Manager, cfg Config) error {
	if cfg.Changes.Phones == nil || cfg.Changes.Softphones == nil {
		return errors.New("devices: no Changes channels")
	}
	phones, err := NewPhoneReconciler(cfg)
	if err != nil {
		return err
	}
	softphones, err := NewSoftphoneReconciler(cfg)
	if err != nil {
		return err
	}
	own := builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))
	pbxConfig := builder.WithPredicates(named(cfg.Namespace, cfg.Names.ConfigMap))

	err = ctrl.NewControllerManagedBy(mgr).
		Named(phoneController).
		For(&v1alpha1.Phone{}, own).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(phones.all), pbxConfig).
		WatchesRawSource(source.Channel(cfg.Changes.Phones, &handler.TypedEnqueueRequestForObject[*v1alpha1.Phone]{})).
		Complete(phones)
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(softphoneController).
		For(&v1alpha1.Softphone{}, own).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(softphones.all), pbxConfig).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(softphones.all),
			builder.WithPredicates(named(cfg.Namespace, cfg.Names.ProjectedSecret))).
		WatchesRawSource(source.Channel(cfg.Changes.Softphones, &handler.TypedEnqueueRequestForObject[*v1alpha1.Softphone]{})).
		Complete(softphones)
}

// named passes only the events of one object.
func named(namespace, name string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == namespace && o.GetName() == name
	})
}

// all enqueues every Phone.
func (r *PhoneReconciler) all(ctx context.Context, _ client.Object) []reconcile.Request {
	var l v1alpha1.PhoneList
	if err := r.cfg.Client.List(ctx, &l, client.InNamespace(r.cfg.Namespace)); err != nil {
		r.log.Error(err, "listing Phones to decide them again")
		return nil
	}
	out := make([]reconcile.Request, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
	}
	return out
}

// all enqueues every Softphone.
func (r *SoftphoneReconciler) all(ctx context.Context, _ client.Object) []reconcile.Request {
	var l v1alpha1.SoftphoneList
	if err := r.cfg.Client.List(ctx, &l, client.InNamespace(r.cfg.Namespace)); err != nil {
		r.log.Error(err, "listing Softphones to decide them again")
		return nil
	}
	out := make([]reconcile.Request, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
	}
	return out
}
