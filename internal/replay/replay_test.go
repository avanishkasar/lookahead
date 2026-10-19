package replay

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/trace"
)

// fakeSandboxController binds every claim of the run to a Sandbox and marks it Ready after
// delay, alternating warm and cold launch types.
func fakeSandboxController(ctx context.Context, t *testing.T, c client.WithWatch, delay time.Duration) {
	w, err := c.Watch(ctx, &extv1beta1.SandboxClaimList{}, client.InNamespace("ns"))
	if err != nil {
		t.Error(err)
		return
	}
	defer w.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			claim, isClaim := ev.Object.(*extv1beta1.SandboxClaim)
			if !isClaim || claim.Status.SandboxStatus.Name != "" || ev.Type == "DELETED" {
				continue
			}
			lt := sandboxv1beta1.SandboxLaunchTypeWarm
			if n%2 == 1 {
				lt = sandboxv1beta1.SandboxLaunchTypeCold
			}
			n++
			time.Sleep(delay)
			sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: claim.Name, Labels: map[string]string{sandboxv1beta1.SandboxLaunchTypeLabel: lt},
			}}
			if err := c.Create(ctx, sb); err != nil {
				t.Error(err)
				return
			}
			claim.Status.SandboxStatus.Name = sb.Name
			meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"})
			if err := c.Status().Update(ctx, claim); err != nil {
				t.Error(err)
				return
			}
		}
	}
}

func TestRunMeasuresClaimsAndCleansUp(t *testing.T) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, sandboxv1beta1.AddToScheme, extv1beta1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	pool := &extv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Spec:       extv1beta1.SandboxWarmPoolSpec{Replicas: ptr.To[int32](3), TemplateRef: extv1beta1.SandboxTemplateRef{Name: "t"}},
		Status:     extv1beta1.SandboxWarmPoolStatus{Replicas: 3, ReadyReplicas: 2},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pool).
		WithStatusSubresource(&extv1beta1.SandboxClaim{}, &extv1beta1.SandboxWarmPool{}).Build()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctrlCtx, stopCtrl := context.WithCancel(ctx)
	defer stopCtrl()
	go fakeSandboxController(ctrlCtx, t, c, 50*time.Millisecond)

	events := []trace.Event{
		{AtMs: 0, Kind: trace.KindHint, Replicas: 2, NeededInMs: 1000, TTLSeconds: 60},
		{AtMs: 10, Kind: trace.KindClaim, HoldMs: 300},
		{AtMs: 20, Kind: trace.KindClaim, HoldMs: 300},
	}
	res, err := Run(ctx, c, events, Config{
		Namespace: "ns", Pool: "p", TraceName: "unit", Strategy: "test", RunID: "r1",
		UseHints: true, ReadyTimeout: 10 * time.Second, SampleInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Claims != 2 || res.Summary.Ready != 2 || res.Summary.ColdStart != 1 {
		t.Fatalf("summary = %+v", res.Summary)
	}
	if res.Summary.P50Ms < 50 {
		t.Fatalf("p50 = %v, want at least the 50ms controller delay", res.Summary.P50Ms)
	}
	if len(res.Samples) < 2 || res.Samples[0].Replicas != 3 || res.Samples[0].SpecReplicas != 3 {
		t.Fatalf("samples = %+v", res.Samples)
	}
	claims := &extv1beta1.SandboxClaimList{}
	hints := &v1alpha1.DemandHintList{}
	if err := c.List(ctx, claims, client.MatchingLabels{RunLabel: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.List(ctx, hints, client.MatchingLabels{RunLabel: "r1"}); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 || len(hints.Items) != 0 {
		t.Fatalf("leftovers: %d claims, %d hints", len(claims.Items), len(hints.Items))
	}
}

func TestRunRequiresPool(t *testing.T) {
	s := runtime.NewScheme()
	_ = extv1beta1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	_, err := Run(context.Background(), c, nil, Config{Namespace: "ns", Pool: "missing", ReadyTimeout: time.Second, SampleInterval: time.Second})
	if err == nil {
		t.Fatal("expected error for a missing pool")
	}
}
