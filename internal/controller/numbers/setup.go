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

// personController is the Person reconciler's name in the manager.
const personController = "person"

// Setup registers the Person reconciler with the manager (6.2.6): it
// watches Persons for a new generation (so its own status writes never
// enqueue one) and the pbx-config ConfigMap, which enqueues every Person:
// any change that matters to one (a ring target created, deleted,
// renumbered or conflicted; a number taken or freed) changes the compiled
// bundle. The Stream reconciler is internal/controller/streamstate's.
func Setup(mgr ctrl.Manager, cfg Config) error {
	people, err := NewPersonReconciler(cfg)
	if err != nil {
		return err
	}
	pbxConfig := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == cfg.Namespace && o.GetName() == cfg.Names.ConfigMap
	}))
	return ctrl.NewControllerManagedBy(mgr).
		Named(personController).
		For(&v1alpha1.Person{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(people.all), pbxConfig).
		Complete(people)
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
