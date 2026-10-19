package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

// DemandHintReconciler deletes DemandHints once they expire, so a hinter that crashes never
// keeps a pool inflated past the hint's TTL.
type DemandHintReconciler struct {
	client.Client
	// Now is the clock; time.Now if nil.
	Now func() time.Time
}

// Reconcile requeues a hint until its expiry and deletes it then.
func (r *DemandHintReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hint := &v1alpha1.DemandHint{}
	if err := r.Get(ctx, req.NamespacedName, hint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !hint.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if remaining := hint.ExpiryTime().Sub(now); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	// Preconditions make sure we delete the hint we evaluated, not a re-created or updated one.
	err := r.Delete(ctx, hint, client.Preconditions{UID: &hint.UID, ResourceVersion: &hint.ResourceVersion})
	switch {
	case err == nil:
		log.FromContext(ctx).V(4).Info("Deleted expired DemandHint")
		return ctrl.Result{}, nil
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case apierrors.IsConflict(err):
		return ctrl.Result{RequeueAfter: conflictRetry}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("delete expired DemandHint: %w", err)
	}
}

// SetupWithManager registers the reconciler.
func (r *DemandHintReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("demandhint").
		For(&v1alpha1.DemandHint{}).
		Complete(r)
}
