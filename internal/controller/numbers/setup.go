package numbers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// Controller names.
const (
	personController = "person"
	streamController = "stream"
)

// Setup registers both reconcilers with the manager (6.2.6): each watches
// its kind for a new generation (so its own status writes never enqueue
// it) and the pbx-config ConfigMap, which enqueues every resource of the
// kind: any change that matters to a Person or Stream (a ring target
// created, deleted, renumbered or conflicted; a number taken or freed)
// changes the compiled bundle.
func Setup(mgr ctrl.Manager, cfg Config) error {
	people, err := NewPersonReconciler(cfg)
	if err != nil {
		return err
	}
	streams, err := NewStreamReconciler(cfg)
	if err != nil {
		return err
	}
	pbxConfig := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == cfg.Namespace && o.GetName() == cfg.Names.ConfigMap
	}))
	err = ctrl.NewControllerManagedBy(mgr).
		Named(personController).
		For(&v1alpha1.Person{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(people.all), pbxConfig).
		Complete(people)
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(streamController).
		For(&v1alpha1.Stream{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(streams.all), pbxConfig).
		Complete(streams)
}

// all enqueues every Person.
func (r *PersonReconciler) all(ctx context.Context, _ client.Object) []reconcile.Request {
	var l v1alpha1.PersonList
	if err := r.cfg.Client.List(ctx, &l, client.InNamespace(r.cfg.Namespace)); err != nil {
		r.log.Error(err, "listing Persons to judge them again")
		return nil
	}
	out := make([]reconcile.Request, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
	}
	return out
}

// all enqueues every Stream.
func (r *StreamReconciler) all(ctx context.Context, _ client.Object) []reconcile.Request {
	var l v1alpha1.StreamList
	if err := r.cfg.Client.List(ctx, &l, client.InNamespace(r.cfg.Namespace)); err != nil {
		r.log.Error(err, "listing Streams to judge them again")
		return nil
	}
	out := make([]reconcile.Request, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
	}
	return out
}
