package inspect

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

const ns = "default"

var t0 = time.Unix(1_000_000, 0)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, extv1beta1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func pool(spec, replicas, ready int32) *extv1beta1.SandboxWarmPool {
	return &extv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bench-pool"},
		Spec:       extv1beta1.SandboxWarmPoolSpec{Replicas: new(spec), TemplateRef: extv1beta1.SandboxTemplateRef{Name: "tmpl"}},
		Status:     extv1beta1.SandboxWarmPoolStatus{Replicas: replicas, ReadyReplicas: ready},
	}
}

func policy(name string, created time.Time, status v1alpha1.LookaheadPolicyStatus) *v1alpha1.LookaheadPolicy {
	return &v1alpha1.LookaheadPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec: v1alpha1.LookaheadPolicySpec{
			PoolRef:         v1alpha1.PoolReference{Name: "bench-pool"},
			MinReplicas:     new(int32(2)),
			MaxReplicas:     60,
			LeadTimeSeconds: new(int32(30)),
		},
		Status: status,
	}
}

func scaling(forecast, hints, active, desired int32) v1alpha1.LookaheadPolicyStatus {
	return v1alpha1.LookaheadPolicyStatus{
		ForecastReplicas: forecast, HintReplicas: hints, ActiveHints: active, DesiredReplicas: desired,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonScaling},
			{Type: v1alpha1.ConditionScaleTargetConflict, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonNoConflict},
		},
	}
}

func hint(name string, replicas int32, neededAt time.Time, ttl int32) *v1alpha1.DemandHint {
	return &v1alpha1.DemandHint{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.DemandHintSpec{PoolRef: v1alpha1.PoolReference{Name: "bench-pool"}, Replicas: replicas,
			NeededAt: &metav1.Time{Time: neededAt}, TTLSeconds: new(ttl)},
	}
}

func claim(name, poolName string, created time.Time, ready bool) *extv1beta1.SandboxClaim {
	c := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       extv1beta1.SandboxClaimSpec{WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: poolName}},
	}
	if ready {
		c.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}}
	}
	return c
}

func TestCollect(t *testing.T) {
	c := newClient(t,
		pool(45, 48, 40),
		policy("newer", t0.Add(-time.Minute), scaling(0, 0, 0, 2)),
		policy("bench-pool", t0.Add(-time.Hour), scaling(5, 40, 1, 45)),
		hint("later", 10, t0.Add(2*time.Minute), 60),
		hint("now", 40, t0.Add(10*time.Second), 90),
		claim("old", "bench-pool", t0.Add(-ClaimWindow-time.Second), true),
		claim("c2", "bench-pool", t0.Add(-5*time.Second), false),
		claim("c1", "bench-pool", t0.Add(-20*time.Second), true),
		claim("other", "other-pool", t0, true),
	)
	s, err := Collect(context.Background(), c, ns, "bench-pool", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !s.PoolFound || s.Spec != 45 || s.Replicas != 48 || s.Ready != 40 {
		t.Errorf("pool = found %v spec %d replicas %d ready %d", s.PoolFound, s.Spec, s.Replicas, s.Ready)
	}
	if s.Policy == nil || s.Policy.Name != "bench-pool" {
		t.Fatalf("policy = %+v, want the oldest policy", s.Policy)
	}
	if p := s.Policy; p.Desired != 45 || p.Forecast != 5 || p.HintReplicas != 40 || !p.Ready || p.Model != "Holt" || p.StabilizationSeconds != 60 {
		t.Errorf("policy view = %+v", p)
	}
	if len(s.Hints) != 2 || s.Hints[0].Name != "now" || s.Hints[1].Name != "later" {
		t.Fatalf("hints = %+v, want sorted by needed time", s.Hints)
	}
	if got := s.Hints[0].CountsFrom; !got.Equal(t0.Add(-20 * time.Second)) {
		t.Errorf("countsFrom = %v, want neededAt minus the 30 s lead", got)
	}
	if s.Hints[0].Phase(t0) != PhaseActive || s.Hints[1].Phase(t0) != PhasePending || s.Hints[0].Phase(t0.Add(time.Hour)) != PhaseExpired {
		t.Errorf("phases wrong")
	}
	if len(s.Claims) != 2 || s.Claims[0].Name != "c1" || s.Claims[1].Ready {
		t.Errorf("claims = %+v, want c1 then c2 (not ready), old and other-pool excluded", s.Claims)
	}
}

func TestCollectMissingPoolAndPolicy(t *testing.T) {
	s, err := Collect(context.Background(), newClient(t), ns, "bench-pool", t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.PoolFound || s.Policy != nil {
		t.Errorf("snapshot = %+v", s)
	}
	if got := Brief(s); !strings.Contains(got, "not found") {
		t.Errorf("brief = %q", got)
	}
}

func TestBrief(t *testing.T) {
	base := func(st v1alpha1.LookaheadPolicyStatus) *Snapshot {
		return &Snapshot{At: t0, Namespace: ns, Pool: "bench-pool", PoolFound: true, Spec: 45, Ready: 40,
			Policy: policyView(policy("bench-pool", t0, st))}
	}
	got := Brief(base(scaling(5, 40, 1, 45)))
	if want := "bench-pool: 40 of 45 warm sandboxes ready; lookahead targets 45 = forecast 5 + hints 40 (1 active), clamped to [2, 60]."; got != want {
		t.Errorf("brief =\n %q\nwant\n %q", got, want)
	}

	held := Brief(base(scaling(3, 0, 0, 45)))
	if !strings.Contains(held, "Holding at 45 through the 60 s scale-down window; demand alone asks for 3.") {
		t.Errorf("holding brief = %q", held)
	}

	pending := base(scaling(5, 0, 0, 5))
	pending.Hints = []Hint{{Name: "rl-step", Replicas: 40, CountsFrom: t0.Add(65 * time.Second)}}
	if got := Brief(pending); !strings.Contains(got, "Next: +40 from rl-step in 1m05s.") {
		t.Errorf("pending brief = %q", got)
	}

	conflict := base(scaling(0, 0, 0, 0))
	conflict.Policy.Conflict = `HorizontalPodAutoscaler "hpa" scales SandboxWarmPool "bench-pool"`
	if got := Brief(conflict); !strings.Contains(got, "lookahead is not scaling it") {
		t.Errorf("conflict brief = %q", got)
	}

	fresh := base(v1alpha1.LookaheadPolicyStatus{})
	if got := Brief(fresh); !strings.Contains(got, "has not been reconciled yet") {
		t.Errorf("unreconciled brief = %q", got)
	}

	none := base(scaling(0, 0, 0, 0))
	none.Policy = nil
	if got := Brief(none); !strings.Contains(got, "No LookaheadPolicy targets this pool") {
		t.Errorf("no-policy brief = %q", got)
	}
}

func TestCard(t *testing.T) {
	s := &Snapshot{At: t0, Namespace: ns, Pool: "bench-pool", PoolFound: true, Spec: 45, Replicas: 45, Ready: 30,
		Policy: policyView(policy("bench-pool", t0, scaling(5, 40, 1, 45))),
		Hints: []Hint{
			{Name: "rl-step", Replicas: 40, CountsFrom: t0.Add(-20 * time.Second), NeededAt: t0.Add(10 * time.Second), Expires: t0.Add(100 * time.Second)},
			{Name: "eval", Replicas: 10, CountsFrom: t0.Add(time.Minute), NeededAt: t0.Add(90 * time.Second), Expires: t0.Add(3 * time.Minute)},
		},
		Claims: []Claim{{Name: "c1", Created: t0.Add(-10 * time.Second)}, {Name: "c2", Created: t0.Add(-2 * time.Minute), Ready: true}},
	}
	h := NewHistory(time.Hour)
	h.Observe(s)
	var b strings.Builder
	if err := Card(&b, s, h, Style{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"╭─ bench-pool  default",
		"30 ready · 45 target · max 60",
		"45 = forecast 5 + hints 40 (1 active) · clamp [2, 60]",
		"Holt · 30 s lead",
		"1 in the last minute · 1 waiting for a sandbox",
		"● rl-step",
		"active   needed in 10s · expires in 1m40s",
		"○ eval",
		"pending  counts in 1m00s · needed in 1m30s",
		"ready  ",
		"per 10s, last 5m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("card without Color has ANSI escapes")
	}
	bar := strings.Count(out, "█") + strings.Count(out, "▒") + strings.Count(out, "░")
	if bar < barWidth {
		t.Errorf("bar has %d cells, want at least %d", bar, barWidth)
	}

	b.Reset()
	if err := Card(&b, s, nil, Style{Color: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\x1b[32m") {
		t.Error("colored card has no green")
	}
}

func TestSpark(t *testing.T) {
	if got := spark([]float64{-1, 0, 4, 8}, 8); got != " ▁▅█" {
		t.Errorf("spark = %q", got)
	}
	if got := spark([]float64{0, 0}, 0); got != "▁▁" {
		t.Errorf("flat spark = %q", got)
	}
	claims := []Claim{{Created: t0.Add(-time.Second)}, {Created: t0.Add(-2 * time.Second)}, {Created: t0.Add(-6 * time.Minute)}}
	got := []rune(claimSpark(claims, t0))
	if len(got) != sparkWidth || got[sparkWidth-1] != '█' || got[0] != '▁' {
		t.Errorf("claimSpark = %q", string(got))
	}
}

func TestShort(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:            "45s",
		65 * time.Second:            "1m05s",
		-90 * time.Second:           "1m30s",
		2*time.Hour + 3*time.Minute: "2h03m",
	} {
		if got := Short(d); got != want {
			t.Errorf("Short(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestHistoryWindow(t *testing.T) {
	h := NewHistory(time.Minute)
	h.Observe(&Snapshot{At: t0, Ready: 1, Claims: []Claim{{Name: "a", Created: t0}},
		Hints: []Hint{{Name: "h", Expires: t0.Add(10 * time.Second)}}})
	h.Observe(&Snapshot{At: t0.Add(30 * time.Second), Ready: 2})
	if len(h.Samples()) != 2 || len(h.Claims()) != 1 || len(h.Hints()) != 1 {
		t.Fatalf("before trim: %d samples %d claims %d hints", len(h.Samples()), len(h.Claims()), len(h.Hints()))
	}
	h.Observe(&Snapshot{At: t0.Add(90 * time.Second), Ready: 3})
	if s := h.Samples(); len(s) != 2 || s[0].Ready != 2 {
		t.Errorf("samples = %+v, want the first trimmed", s)
	}
	if len(h.Claims()) != 0 || len(h.Hints()) != 0 {
		t.Errorf("claims and hints older than the window should be dropped")
	}
}
