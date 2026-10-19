// Package controller contains the lookahead reconcilers.
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/arrivals"
	"github.com/avanishkasar/lookahead/internal/forecast"
)

// FieldManager is the field manager name used for every write lookahead makes.
const FieldManager = "lookahead-controller"

// conflictRetry is the delay before re-reading an object whose write hit a resourceVersion
// conflict.
const conflictRetry = 200 * time.Millisecond

// defaultPoolReplicas mirrors the +kubebuilder:default on SandboxWarmPool spec.replicas.
const defaultPoolReplicas int32 = 1

// +kubebuilder:rbac:groups=lookahead.avanishkasar.github.io,resources=lookaheadpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=lookahead.avanishkasar.github.io,resources=lookaheadpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lookahead.avanishkasar.github.io,resources=demandhints,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch

// LookaheadPolicyReconciler sets a SandboxWarmPool's spec.replicas from a claim-arrival forecast
// plus active DemandHints.
type LookaheadPolicyReconciler struct {
	client.Client
	Arrivals   *arrivals.Recorder
	Stabilizer *forecast.Stabilizer
	// Now is the clock; time.Now if nil.
	Now func() time.Time
}

// settings is a LookaheadPolicySpec with defaults resolved. CRD defaulting normally fills
// these, but objects written before a default existed (or by tests) may lack them.
type settings struct {
	min, max   int32
	leadSec    int32
	bucketSec  int32
	window     time.Duration
	history    time.Duration
	params     forecast.Params
	safetyZ    float64
	noForecast bool
	leadTime   time.Duration
	bucketSize time.Duration
}

func resolve(spec v1alpha1.LookaheadPolicySpec) settings {
	f := spec.Forecast
	s := settings{
		min:       ptr.Deref(spec.MinReplicas, 0),
		max:       spec.MaxReplicas,
		leadSec:   ptr.Deref(spec.LeadTimeSeconds, 30),
		bucketSec: ptr.Deref(f.BucketSeconds, 5),
		window:    time.Duration(ptr.Deref(spec.ScaleDownStabilizationSeconds, 60)) * time.Second,
		history:   time.Duration(ptr.Deref(f.HistorySeconds, 1800)) * time.Second,
		params: forecast.Params{
			Alpha: float64(ptr.Deref(f.AlphaPercent, 50)) / 100,
			Beta:  float64(ptr.Deref(f.BetaPercent, 10)) / 100,
		},
		safetyZ: float64(ptr.Deref(f.SafetyFactorPercent, 100)) / 100,
	}
	switch f.Model {
	case v1alpha1.ForecastModelEWMA:
		s.params.Beta = 0
	case v1alpha1.ForecastModelNone:
		s.noForecast = true
	}
	if s.bucketSec < 1 {
		s.bucketSec = 1
	}
	s.leadTime = time.Duration(s.leadSec) * time.Second
	s.bucketSize = time.Duration(s.bucketSec) * time.Second
	return s
}

// recommendation is one sizing decision, before stabilization.
type recommendation struct {
	forecastReplicas int32
	hintReplicas     int64
	activeHints      int32
	raw              int32
	// nextHintChange is the earliest future time a hint starts or stops counting, or zero.
	nextHintChange time.Time
}

func (r *LookaheadPolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile computes the pool target and writes it unless another owner of the pool's replicas
// exists.
func (r *LookaheadPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	stabKey := req.String()

	policy := &v1alpha1.LookaheadPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			r.Stabilizer.Forget(stabKey)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get LookaheadPolicy: %w", err)
	}
	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	now := r.now()
	s := resolve(policy.Spec)
	original := policy.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation

	pool := &extv1beta1.SandboxWarmPool{}
	poolKey := types.NamespacedName{Namespace: policy.Namespace, Name: policy.Spec.PoolRef.Name}
	if err := r.Get(ctx, poolKey, pool); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get SandboxWarmPool: %w", err)
		}
		// The pool watch re-triggers this policy when the pool appears.
		setCondition(policy, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPoolNotFound,
			fmt.Sprintf("SandboxWarmPool %q not found", poolKey.Name))
		return ctrl.Result{}, r.patchStatus(ctx, original, policy)
	}

	rec, err := r.recommend(ctx, policy, s, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	// After a restart the stabilizer is empty; seed it with the last published target so a
	// restart never causes an immediate scale-down that the window would have prevented.
	if !r.Stabilizer.Seeded(stabKey) && policy.Status.DesiredReplicas > 0 {
		r.Stabilizer.Recommend(stabKey, now, policy.Status.DesiredReplicas, s.window)
	}
	target := forecast.Clamp(int64(r.Stabilizer.Recommend(stabKey, now, rec.raw, s.window)), int64(s.min), int64(s.max))

	policy.Status.ForecastReplicas = rec.forecastReplicas
	policy.Status.HintReplicas = int32(min(rec.hintReplicas, int64(s.max)))
	policy.Status.ActiveHints = rec.activeHints
	policy.Status.DesiredReplicas = target

	conflictReason, conflictMsg, err := r.findConflict(ctx, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflictReason != "" {
		setCondition(policy, v1alpha1.ConditionScaleTargetConflict, metav1.ConditionTrue, conflictReason, conflictMsg)
		setCondition(policy, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonConflict,
			"not writing spec.replicas: "+conflictMsg)
		if err := r.patchStatus(ctx, original, policy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.requeueAfter(now, s, rec)}, nil
	}
	setCondition(policy, v1alpha1.ConditionScaleTargetConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, "")

	current := ptr.Deref(pool.Spec.Replicas, defaultPoolReplicas)
	if current != target {
		patch := client.MergeFromWithOptions(pool.DeepCopy(), client.MergeFromWithOptimisticLock{})
		pool.Spec.Replicas = new(target)
		if err := r.Patch(ctx, pool, patch, client.FieldOwner(FieldManager)); err != nil {
			if apierrors.IsConflict(err) {
				// The pool changed under us; recompute against the fresh object.
				return ctrl.Result{RequeueAfter: conflictRetry}, nil
			}
			setCondition(policy, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonScaleUpdateFailure, err.Error())
			if perr := r.patchStatus(ctx, original, policy); perr != nil {
				logger.Error(perr, "Failed to patch LookaheadPolicy status")
			}
			return ctrl.Result{}, fmt.Errorf("patch SandboxWarmPool replicas: %w", err)
		}
		policy.Status.LastScaleTime = &metav1.Time{Time: now}
		logger.Info("Scaled SandboxWarmPool", "pool", pool.Name, "from", current, "to", target,
			"forecast", rec.forecastReplicas, "hints", rec.hintReplicas)
	}
	setCondition(policy, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonScaling,
		fmt.Sprintf("pool target %d (forecast %d + hints %d, bounds [%d, %d])",
			target, rec.forecastReplicas, rec.hintReplicas, s.min, s.max))
	if err := r.patchStatus(ctx, original, policy); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.requeueAfter(now, s, rec)}, nil
}

func (r *LookaheadPolicyReconciler) recommend(ctx context.Context, policy *v1alpha1.LookaheadPolicy, s settings, now time.Time) (recommendation, error) {
	var rec recommendation
	if !s.noForecast {
		key := arrivals.Key{Namespace: policy.Namespace, Pool: policy.Spec.PoolRef.Name}
		series := r.Arrivals.Series(key, now, s.bucketSize, s.history)
		level, trend := forecast.Smooth(series, s.params)
		expected := forecast.ExpectedArrivals(level, trend, forecast.HorizonBuckets(s.leadSec, s.bucketSec))
		rec.forecastReplicas = forecast.Replicas(expected, s.safetyZ)
	}

	hints := &v1alpha1.DemandHintList{}
	if err := r.List(ctx, hints, client.InNamespace(policy.Namespace),
		client.MatchingFields{v1alpha1.PoolRefField: policy.Spec.PoolRef.Name}); err != nil {
		return rec, fmt.Errorf("list DemandHints: %w", err)
	}
	for i := range hints.Items {
		h := &hints.Items[i]
		if !h.DeletionTimestamp.IsZero() {
			continue
		}
		start, end := h.NeededTime().Add(-s.leadTime), h.ExpiryTime()
		if h.ActiveAt(now, s.leadTime) {
			rec.hintReplicas += int64(h.Spec.Replicas)
			rec.activeHints++
			rec.nextHintChange = earliest(rec.nextHintChange, end)
		} else if now.Before(start) {
			rec.nextHintChange = earliest(rec.nextHintChange, start)
		}
	}
	rec.raw = forecast.Clamp(int64(rec.forecastReplicas)+rec.hintReplicas, int64(s.min), int64(s.max))
	return rec, nil
}

// findConflict returns a non-empty reason if something else owns the pool's replicas.
func (r *LookaheadPolicyReconciler) findConflict(ctx context.Context, policy *v1alpha1.LookaheadPolicy) (string, string, error) {
	hpas := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := r.List(ctx, hpas, client.InNamespace(policy.Namespace)); err != nil {
		return "", "", fmt.Errorf("list HorizontalPodAutoscalers: %w", err)
	}
	for i := range hpas.Items {
		if hpaTargetsPool(&hpas.Items[i], policy.Spec.PoolRef.Name) {
			return v1alpha1.ReasonHPATargetsPool,
				fmt.Sprintf("HorizontalPodAutoscaler %q scales SandboxWarmPool %q", hpas.Items[i].Name, policy.Spec.PoolRef.Name), nil
		}
	}

	policies := &v1alpha1.LookaheadPolicyList{}
	if err := r.List(ctx, policies, client.InNamespace(policy.Namespace),
		client.MatchingFields{v1alpha1.PoolRefField: policy.Spec.PoolRef.Name}); err != nil {
		return "", "", fmt.Errorf("list LookaheadPolicies: %w", err)
	}
	for i := range policies.Items {
		other := &policies.Items[i]
		if other.UID == policy.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if olderThan(other, policy) {
			return v1alpha1.ReasonOlderPolicyExists,
				fmt.Sprintf("LookaheadPolicy %q already targets SandboxWarmPool %q", other.Name, policy.Spec.PoolRef.Name), nil
		}
	}
	return "", "", nil
}

func hpaTargetsPool(hpa *autoscalingv2.HorizontalPodAutoscaler, pool string) bool {
	ref := hpa.Spec.ScaleTargetRef
	if ref.Kind != extv1beta1.SandboxWarmPoolKind || ref.Name != pool {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == extv1beta1.GroupVersion.Group
}

// olderThan orders policies by creation time, then name, so exactly one policy per pool wins.
func olderThan(a, b *v1alpha1.LookaheadPolicy) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return strings.Compare(a.Name, b.Name) < 0
}

// requeueAfter wakes the policy at the next bucket boundary (when the forecast can change) or
// the next hint transition, whichever is first.
func (r *LookaheadPolicyReconciler) requeueAfter(now time.Time, s settings, rec recommendation) time.Duration {
	next := now.Truncate(s.bucketSize).Add(s.bucketSize)
	next = earliest(next, rec.nextHintChange)
	return max(next.Sub(now), 100*time.Millisecond)
}

func (r *LookaheadPolicyReconciler) patchStatus(ctx context.Context, original, policy *v1alpha1.LookaheadPolicy) error {
	// Policies requeue every bucket; skip the API call when nothing changed.
	if equality.Semantic.DeepEqual(original.Status, policy.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, policy, client.MergeFrom(original), client.FieldOwner(FieldManager)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("patch LookaheadPolicy status: %w", err)
	}
	return nil
}

func setCondition(policy *v1alpha1.LookaheadPolicy, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: policy.Generation,
	})
}

func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	default:
		return a
	}
}

// IndexPoolRef registers the poolRef field index for LookaheadPolicy and DemandHint.
func IndexPoolRef(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.LookaheadPolicy{}, v1alpha1.PoolRefField,
		func(o client.Object) []string { return []string{o.(*v1alpha1.LookaheadPolicy).Spec.PoolRef.Name} }); err != nil {
		return fmt.Errorf("index LookaheadPolicy poolRef: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.DemandHint{}, v1alpha1.PoolRefField,
		func(o client.Object) []string { return []string{o.(*v1alpha1.DemandHint).Spec.PoolRef.Name} }); err != nil {
		return fmt.Errorf("index DemandHint poolRef: %w", err)
	}
	return nil
}

// policiesForPool maps an object naming a pool to the policies targeting that pool.
func (r *LookaheadPolicyReconciler) policiesForPool(ctx context.Context, namespace, pool string) []reconcile.Request {
	policies := &v1alpha1.LookaheadPolicyList{}
	if err := r.List(ctx, policies, client.InNamespace(namespace), client.MatchingFields{v1alpha1.PoolRefField: pool}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list LookaheadPolicies for pool", "namespace", namespace, "pool", pool)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(policies.Items))
	for i := range policies.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&policies.Items[i])})
	}
	return reqs
}

// claimArrivalHandler records every SandboxClaim the informer reports as added. The informer's
// initial list replays existing claims as adds, which rebuilds history after a restart. It does
// not enqueue: the forecast only uses completed buckets, and policies requeue at bucket
// boundaries.
func claimArrivalHandler(rec *arrivals.Recorder) handler.TypedEventHandler[client.Object, reconcile.Request] {
	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.CreateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			claim, ok := e.Object.(*extv1beta1.SandboxClaim)
			if !ok || claim.Spec.WarmPoolRef.Name == "" {
				return
			}
			rec.Record(arrivals.Key{Namespace: claim.Namespace, Pool: claim.Spec.WarmPoolRef.Name}, claim.CreationTimestamp.Time)
		},
	}
}

// SetupWithManager registers the reconciler and its watches. IndexPoolRef must run first.
func (r *LookaheadPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("lookaheadpolicy").
		For(&v1alpha1.LookaheadPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&extv1beta1.SandboxWarmPool{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				return r.policiesForPool(ctx, o.GetNamespace(), o.GetName())
			}), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.DemandHint{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				return r.policiesForPool(ctx, o.GetNamespace(), o.(*v1alpha1.DemandHint).Spec.PoolRef.Name)
			})).
		Watches(&autoscalingv2.HorizontalPodAutoscaler{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				hpa := o.(*autoscalingv2.HorizontalPodAutoscaler)
				if !hpaTargetsPool(hpa, hpa.Spec.ScaleTargetRef.Name) {
					return nil
				}
				return r.policiesForPool(ctx, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name)
			})).
		Watches(&extv1beta1.SandboxClaim{}, claimArrivalHandler(r.Arrivals)).
		Complete(r)
}
