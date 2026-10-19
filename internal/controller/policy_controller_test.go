package controller

import (
	"context"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/arrivals"
	"github.com/avanishkasar/lookahead/internal/forecast"
)

const ns = "default"

// t0 is aligned to a 5s bucket so bucket arithmetic in tests is exact.
var t0 = time.Unix(1_000_000, 0)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, extv1beta1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.LookaheadPolicy{}).
		WithIndex(&v1alpha1.LookaheadPolicy{}, v1alpha1.PoolRefField, func(o client.Object) []string {
			return []string{o.(*v1alpha1.LookaheadPolicy).Spec.PoolRef.Name}
		}).
		WithIndex(&v1alpha1.DemandHint{}, v1alpha1.PoolRefField, func(o client.Object) []string {
			return []string{o.(*v1alpha1.DemandHint).Spec.PoolRef.Name}
		}).
		Build()
}

func pool(name string, replicas int32) *extv1beta1.SandboxWarmPool {
	return &extv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: extv1beta1.SandboxWarmPoolSpec{
			Replicas:    new(replicas),
			TemplateRef: extv1beta1.SandboxTemplateRef{Name: "tmpl"},
		},
	}
}

func policy(name, poolName string, minR, maxR int32, created time.Time) *v1alpha1.LookaheadPolicy {
	return &v1alpha1.LookaheadPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name), CreationTimestamp: metav1.NewTime(created)},
		Spec: v1alpha1.LookaheadPolicySpec{
			PoolRef:     v1alpha1.PoolReference{Name: poolName},
			MinReplicas: new(minR),
			MaxReplicas: maxR,
		},
	}
}

func hint(name, poolName string, replicas int32, neededAt time.Time, ttl int32) *v1alpha1.DemandHint {
	return &v1alpha1.DemandHint{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name), CreationTimestamp: metav1.NewTime(neededAt.Add(-time.Hour))},
		Spec: v1alpha1.DemandHintSpec{
			PoolRef:    v1alpha1.PoolReference{Name: poolName},
			Replicas:   replicas,
			NeededAt:   &metav1.Time{Time: neededAt},
			TTLSeconds: new(ttl),
		},
	}
}

type harness struct {
	c   client.Client
	r   *LookaheadPolicyReconciler
	now time.Time
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	h := &harness{c: newClient(t, objs...), now: t0}
	clock := func() time.Time { return h.now }
	h.r = &LookaheadPolicyReconciler{
		Client:     h.c,
		Arrivals:   arrivals.NewRecorder(clock),
		Stabilizer: forecast.NewStabilizer(),
		Now:        clock,
	}
	return h
}

func (h *harness) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// poolReplicas returns spec.replicas of the pool every test uses, "p".
func (h *harness) poolReplicas(t *testing.T) int32 {
	t.Helper()
	p := &extv1beta1.SandboxWarmPool{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "p"}, p); err != nil {
		t.Fatal(err)
	}
	return ptr.Deref(p.Spec.Replicas, -1)
}

func (h *harness) policy(t *testing.T, name string) *v1alpha1.LookaheadPolicy {
	t.Helper()
	p := &v1alpha1.LookaheadPolicy{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// steady records perBucket arrivals in every 5s bucket of the last 30 minutes.
func (h *harness) steady(poolName string, perBucket int) {
	k := arrivals.Key{Namespace: ns, Pool: poolName}
	for b := 1; b <= 360; b++ {
		for range perBucket {
			h.r.Arrivals.Record(k, h.now.Add(-time.Duration(b)*5*time.Second))
		}
	}
}

func assertCondition(t *testing.T, p *v1alpha1.LookaheadPolicy, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(p.Status.Conditions, typ)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Fatalf("condition %s = %+v, want status %s reason %s", typ, c, status, reason)
	}
}

func TestForecastScalesPool(t *testing.T) {
	h := newHarness(t, pool("p", 1), policy("lap", "p", 0, 100, t0))
	h.steady("p", 2)

	res := h.reconcile(t, "lap")

	// level 2/bucket, flat trend, 30s lead = 6 buckets -> 12 expected; z=1 -> ceil(12+sqrt(12)) = 16
	if got := h.poolReplicas(t); got != 16 {
		t.Fatalf("pool replicas = %d, want 16", got)
	}
	st := h.policy(t, "lap").Status
	if st.ForecastReplicas != 16 || st.DesiredReplicas != 16 || st.HintReplicas != 0 || st.LastScaleTime == nil {
		t.Fatalf("status = %+v", st)
	}
	assertCondition(t, h.policy(t, "lap"), v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonScaling)
	assertCondition(t, h.policy(t, "lap"), v1alpha1.ConditionScaleTargetConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict)
	if res.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %v, want next bucket boundary 5s", res.RequeueAfter)
	}
}

func TestEWMAAndSafetyFactor(t *testing.T) {
	p := policy("lap", "p", 0, 100, t0)
	p.Spec.Forecast = v1alpha1.ForecastSpec{Model: v1alpha1.ForecastModelEWMA, SafetyFactorPercent: ptr.To[int32](0)}
	h := newHarness(t, pool("p", 1), p)
	h.steady("p", 2)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 12 {
		t.Fatalf("pool replicas = %d, want 12 (no headroom)", got)
	}
}

func TestModelNoneUsesHintsOnly(t *testing.T) {
	p := policy("lap", "p", 0, 100, t0)
	p.Spec.Forecast.Model = v1alpha1.ForecastModelNone
	h := newHarness(t, pool("p", 1), p, hint("h", "p", 4, t0, 60))
	h.steady("p", 2)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 4 {
		t.Fatalf("pool replicas = %d, want 4 (hints only)", got)
	}
}

func TestHintsAddToForecastAndRespectWindow(t *testing.T) {
	h := newHarness(t,
		pool("p", 1),
		policy("lap", "p", 0, 100, t0),
		hint("active", "p", 10, t0.Add(20*time.Second), 60), // starts at t0-10s (lead 30s)
		hint("future", "p", 7, t0.Add(90*time.Second), 60),  // starts at t0+60s
		hint("expired", "p", 5, t0.Add(-2*time.Minute), 60), // ended at t0-60s
		hint("other", "q", 50, t0, 60),                      // other pool
	)
	res := h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 10 {
		t.Fatalf("pool replicas = %d, want 10", got)
	}
	st := h.policy(t, "lap").Status
	if st.ActiveHints != 1 || st.HintReplicas != 10 {
		t.Fatalf("status = %+v", st)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %v", res.RequeueAfter)
	}

	// At the future hint's start both count.
	h.now = t0.Add(60 * time.Second)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 17 {
		t.Fatalf("pool replicas = %d, want 17", got)
	}
}

func TestRequeueAtHintTransitionBeforeBucket(t *testing.T) {
	h := newHarness(t, pool("p", 0), policy("lap", "p", 0, 100, t0),
		hint("soon", "p", 3, t0.Add(32*time.Second), 60)) // starts at t0+2s
	res := h.reconcile(t, "lap")
	if res.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want 2s", res.RequeueAfter)
	}
}

func TestClampToMinAndMax(t *testing.T) {
	h := newHarness(t, pool("p", 1), policy("lap", "p", 3, 20, t0), hint("big", "p", 500, t0, 60))
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 20 {
		t.Fatalf("pool replicas = %d, want max 20", got)
	}
	if st := h.policy(t, "lap").Status; st.HintReplicas != 20 {
		t.Fatalf("hintReplicas = %d, want capped at 20", st.HintReplicas)
	}

	h2 := newHarness(t, pool("p", 1), policy("lap", "p", 3, 20, t0))
	h2.reconcile(t, "lap")
	if got := h2.poolReplicas(t); got != 3 {
		t.Fatalf("pool replicas = %d, want min 3", got)
	}
}

func TestScaleDownIsStabilized(t *testing.T) {
	p := policy("lap", "p", 0, 100, t0)
	p.Spec.ScaleDownStabilizationSeconds = ptr.To[int32](60)
	h := newHarness(t, pool("p", 0), p, hint("burst", "p", 10, t0, 10)) // counts until t0+10s
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 10 {
		t.Fatalf("pool replicas = %d, want 10", got)
	}
	h.now = t0.Add(30 * time.Second)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 10 {
		t.Fatalf("pool shrank inside the window: %d", got)
	}
	h.now = t0.Add(61 * time.Second)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 0 {
		t.Fatalf("pool replicas = %d, want 0 after the window", got)
	}
}

func TestRestartDoesNotDropTargetInsideWindow(t *testing.T) {
	p := policy("lap", "p", 0, 100, t0)
	p.Status.DesiredReplicas = 12
	h := newHarness(t, pool("p", 12), p) // fresh stabilizer and recorder, as after a restart
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 12 {
		t.Fatalf("pool replicas = %d, want 12 kept after restart", got)
	}
}

func TestHPAConflictDoesNotWrite(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "keda-hpa-p"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1", Kind: "SandboxWarmPool", Name: "p",
			},
			MaxReplicas: 10,
		},
	}
	h := newHarness(t, pool("p", 4), policy("lap", "p", 0, 100, t0), hint("h", "p", 9, t0, 60), hpa)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 4 {
		t.Fatalf("pool replicas = %d, want untouched 4", got)
	}
	p := h.policy(t, "lap")
	assertCondition(t, p, v1alpha1.ConditionScaleTargetConflict, metav1.ConditionTrue, v1alpha1.ReasonHPATargetsPool)
	assertCondition(t, p, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonConflict)
	if p.Status.DesiredReplicas != 9 {
		t.Fatalf("desiredReplicas = %d, want 9 still reported", p.Status.DesiredReplicas)
	}
}

func TestHPAForOtherKindIsNotAConflict(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "p"},
			MaxReplicas:    10,
		},
	}
	h := newHarness(t, pool("p", 1), policy("lap", "p", 2, 100, t0), hpa)
	h.reconcile(t, "lap")
	if got := h.poolReplicas(t); got != 2 {
		t.Fatalf("pool replicas = %d, want 2", got)
	}
}

func TestOlderPolicyWins(t *testing.T) {
	h := newHarness(t, pool("p", 1),
		policy("old", "p", 5, 100, t0.Add(-time.Hour)),
		policy("new", "p", 9, 100, t0))
	h.reconcile(t, "new")
	if got := h.poolReplicas(t); got != 1 {
		t.Fatalf("newer policy wrote replicas: %d", got)
	}
	assertCondition(t, h.policy(t, "new"), v1alpha1.ConditionScaleTargetConflict, metav1.ConditionTrue, v1alpha1.ReasonOlderPolicyExists)
	h.reconcile(t, "old")
	if got := h.poolReplicas(t); got != 5 {
		t.Fatalf("older policy did not write: %d", got)
	}
}

func TestPoolNotFound(t *testing.T) {
	h := newHarness(t, policy("lap", "missing", 0, 10, t0))
	res := h.reconcile(t, "lap")
	assertCondition(t, h.policy(t, "lap"), v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPoolNotFound)
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want 0 (pool watch re-triggers)", res.RequeueAfter)
	}
}

func TestDeletedPolicyIsForgotten(t *testing.T) {
	h := newHarness(t)
	h.r.Stabilizer.Recommend(ns+"/lap", t0, 5, time.Minute)
	h.reconcile(t, "lap")
	if h.r.Stabilizer.Seeded(ns + "/lap") {
		t.Fatal("stabilizer state kept for a deleted policy")
	}
}

func TestUnchangedTargetDoesNotWrite(t *testing.T) {
	h := newHarness(t, pool("p", 3), policy("lap", "p", 3, 10, t0))
	h.reconcile(t, "lap")
	if h.policy(t, "lap").Status.LastScaleTime != nil {
		t.Fatal("lastScaleTime set without a scale")
	}
}

func TestUnchangedStatusIsNotPatched(t *testing.T) {
	h := newHarness(t, pool("p", 3), policy("lap", "p", 3, 10, t0))
	h.reconcile(t, "lap")
	before := h.policy(t, "lap").ResourceVersion
	h.reconcile(t, "lap")
	if after := h.policy(t, "lap").ResourceVersion; after != before {
		t.Fatalf("status patched without a change: resourceVersion %s -> %s", before, after)
	}
}

func TestClaimArrivalHandlerRecords(t *testing.T) {
	rec := arrivals.NewRecorder(func() time.Time { return t0 })
	hd := claimArrivalHandler(rec)
	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c", CreationTimestamp: metav1.NewTime(t0.Add(-3 * time.Second))},
		Spec:       extv1beta1.SandboxClaimSpec{WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: "p"}},
	}
	hd.Create(context.Background(), event.CreateEvent{Object: claim}, nil)
	hd.Create(context.Background(), event.CreateEvent{Object: pool("not-a-claim", 1)}, nil)
	got := rec.Series(arrivals.Key{Namespace: ns, Pool: "p"}, t0, 5*time.Second, 5*time.Second)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("Series = %v, want [1]", got)
	}
}
