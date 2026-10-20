package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

const ns = "lookahead-bench"

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

func fixtures() []client.Object {
	return []client.Object{
		&extv1beta1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bench-pool"},
			Spec:       extv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(5)), TemplateRef: extv1beta1.SandboxTemplateRef{Name: "tmpl"}},
			Status:     extv1beta1.SandboxWarmPoolStatus{Replicas: 5, ReadyReplicas: 5},
		},
		&v1alpha1.LookaheadPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bench-pool"},
			Spec: v1alpha1.LookaheadPolicySpec{PoolRef: v1alpha1.PoolReference{Name: "bench-pool"},
				MinReplicas: new(int32(2)), MaxReplicas: 60, LeadTimeSeconds: new(int32(30))},
			Status: v1alpha1.LookaheadPolicyStatus{ForecastReplicas: 5, DesiredReplicas: 5,
				Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonScaling}}},
		},
	}
}

// connect starts the server on an in-memory transport and returns a client session.
func connect(t *testing.T, c client.Client) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := New(c, Config{Namespace: ns, Version: "test", Now: func() time.Time { return t0 }})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestListTools(t *testing.T) {
	cs := connect(t, newClient(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, name := range []string{"pool_status", "create_hint", "list_hints", "delete_hint"} {
		if got[name] == nil {
			t.Errorf("missing tool %s", name)
		}
	}
	if a := got["pool_status"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Error("pool_status should be read-only")
	}
	if init := cs.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "create_hint") {
		t.Error("server instructions should tell agents when to send hints")
	}
}

func TestPoolStatus(t *testing.T) {
	cs := connect(t, newClient(t, fixtures()...))
	out, isErr := call(t, cs, "pool_status", map[string]any{})
	if isErr {
		t.Fatalf("tool error: %s", out)
	}
	for _, want := range []string{"bench-pool: 5 of 5 warm sandboxes ready; lookahead targets 5", "```text", "╭─ bench-pool", "5 = forecast 5 + hints 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("pool_status missing %q:\n%s", want, out)
		}
	}
}

func TestCreateListDeleteHint(t *testing.T) {
	c := newClient(t, fixtures()...)
	cs := connect(t, c)

	out, isErr := call(t, cs, "create_hint", map[string]any{"pool": "bench-pool", "replicas": 40, "in_seconds": 45, "ttl_seconds": 90, "name": "rl-step"})
	if isErr {
		t.Fatalf("tool error: %s", out)
	}
	for _, want := range []string{"Created demandhint/rl-step: +40 sandboxes for bench-pool", "starts counting it in 15s (30 s lead", "○ rl-step"} {
		if !strings.Contains(out, want) {
			t.Errorf("create_hint missing %q:\n%s", want, out)
		}
	}
	h := &v1alpha1.DemandHint{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "rl-step"}, h); err != nil {
		t.Fatal(err)
	}
	if h.Spec.Replicas != 40 || *h.Spec.TTLSeconds != 90 || !h.NeededTime().Equal(t0.Add(45*time.Second)) {
		t.Errorf("created hint = %+v", h.Spec)
	}

	out, _ = call(t, cs, "list_hints", map[string]any{})
	if !strings.Contains(out, "rl-step") || !strings.Contains(out, "pending") {
		t.Errorf("list_hints:\n%s", out)
	}

	out, isErr = call(t, cs, "delete_hint", map[string]any{"name": "rl-step"})
	if isErr || !strings.Contains(out, "Deleted demandhint/rl-step") {
		t.Errorf("delete_hint: %s", out)
	}
	if out, _ = call(t, cs, "list_hints", map[string]any{}); !strings.Contains(out, "No DemandHints") {
		t.Errorf("list_hints after delete:\n%s", out)
	}
}

func TestCreateHintErrors(t *testing.T) {
	cs := connect(t, newClient(t, fixtures()...))
	if out, isErr := call(t, cs, "create_hint", map[string]any{"pool": "bench-pool", "replicas": 0}); !isErr || !strings.Contains(out, "replicas") {
		t.Errorf("zero replicas: isErr %v, %s", isErr, out)
	}
	if out, isErr := call(t, cs, "create_hint", map[string]any{"pool": "missing", "replicas": 3}); !isErr || !strings.Contains(out, "not found") {
		t.Errorf("missing pool: isErr %v, %s", isErr, out)
	}
}

func TestCreateHintWithoutPolicyWarns(t *testing.T) {
	cs := connect(t, newClient(t, fixtures()[0]))
	out, isErr := call(t, cs, "create_hint", map[string]any{"pool": "bench-pool", "replicas": 3})
	if isErr || !strings.Contains(out, "no LookaheadPolicy targets this pool") {
		t.Errorf("isErr %v:\n%s", isErr, out)
	}
}
