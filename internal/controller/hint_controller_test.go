package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

func TestHintGC(t *testing.T) {
	c := newClient(t, hint("h", "p", 1, t0, 60)) // expires at t0+60s
	now := t0
	r := &DemandHintReconciler{Client: c, Now: func() time.Time { return now }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "h"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil || res.RequeueAfter != 60*time.Second {
		t.Fatalf("before expiry: res=%+v err=%v, want requeue 60s", res, err)
	}

	now = t0.Add(60 * time.Second)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("at expiry: %v", err)
	}
	if err := c.Get(context.Background(), req.NamespacedName, &v1alpha1.DemandHint{}); !apierrors.IsNotFound(err) {
		t.Fatalf("hint not deleted: %v", err)
	}

	// Already gone: no error.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("missing hint: %v", err)
	}
}

func TestHintDefaults(t *testing.T) {
	h := hint("h", "p", 1, t0, 0)
	h.Spec.NeededAt = nil
	h.Spec.TTLSeconds = nil
	h.CreationTimestamp.Time = t0
	if got := h.ExpiryTime(); !got.Equal(t0.Add(time.Duration(v1alpha1.DefaultHintTTLSeconds) * time.Second)) {
		t.Fatalf("ExpiryTime = %v", got)
	}
	if !h.ActiveAt(t0, 30*time.Second) {
		t.Fatal("hint without neededAt should be active at creation")
	}
}
