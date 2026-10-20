// Package inspect reads the live state of lookahead-managed pools and renders it for people and
// agents: a one-line brief, a text card for terminals and MCP results, and JSON for the dashboard.
package inspect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

// ClaimWindow is how far back a Snapshot lists claims.
const ClaimWindow = 10 * time.Minute

// Defaults mirror the LookaheadPolicy CRD defaults the controller resolves.
const (
	defaultLeadSeconds          int32 = 30
	defaultStabilizationSeconds int32 = 60
	defaultPoolReplicas         int32 = 1
)

// Snapshot is the state of one SandboxWarmPool and what lookahead is doing to it.
type Snapshot struct {
	At        time.Time `json:"at"`
	Namespace string    `json:"namespace"`
	Pool      string    `json:"pool"`
	PoolFound bool      `json:"poolFound"`
	// Spec is the pool's spec.replicas: the size lookahead (or someone else) asked for.
	Spec int32 `json:"specReplicas"`
	// Replicas and Ready count the pool's unclaimed sandboxes, and the Ready ones among them.
	Replicas int32 `json:"replicas"`
	Ready    int32 `json:"readyReplicas"`
	// Policy is the LookaheadPolicy that owns the pool, or nil.
	Policy *Policy `json:"policy,omitempty"`
	Hints  []Hint  `json:"hints"`
	// Claims are the pool's claims created within ClaimWindow, oldest first.
	Claims []Claim `json:"claims"`
}

// Policy is the part of a LookaheadPolicy that explains the pool target.
type Policy struct {
	Name                 string `json:"name"`
	Min                  int32  `json:"minReplicas"`
	Max                  int32  `json:"maxReplicas"`
	LeadSeconds          int32  `json:"leadTimeSeconds"`
	StabilizationSeconds int32  `json:"scaleDownStabilizationSeconds"`
	Model                string `json:"model"`
	Forecast             int32  `json:"forecastReplicas"`
	HintReplicas         int32  `json:"hintReplicas"`
	ActiveHints          int32  `json:"activeHints"`
	Desired              int32  `json:"desiredReplicas"`
	// Reconciled is false until the controller has set the Ready condition.
	Reconciled bool   `json:"reconciled"`
	Ready      bool   `json:"ready"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	// Conflict is non-empty when another owner of the pool's replicas stops the policy writing.
	Conflict  string     `json:"conflict,omitempty"`
	LastScale *time.Time `json:"lastScaleTime,omitempty"`
}

// Raw is the target before scale-down stabilization: forecast plus hints, clamped.
func (p *Policy) Raw() int32 {
	return min(max(p.Forecast+p.HintReplicas, p.Min), p.Max)
}

// Holding reports whether stabilization keeps the target above what demand asks for now.
func (p *Policy) Holding() bool {
	return p.Desired > p.Raw()
}

// Hint is a DemandHint with its counting window resolved against the policy's lead time.
type Hint struct {
	Name       string    `json:"name"`
	Replicas   int32     `json:"replicas"`
	CountsFrom time.Time `json:"countsFrom"`
	NeededAt   time.Time `json:"neededAt"`
	Expires    time.Time `json:"expires"`
}

// Phase is where a hint is in its life.
type Phase string

const (
	PhasePending Phase = "pending"
	PhaseActive  Phase = "active"
	PhaseExpired Phase = "expired"
)

// Phase returns the hint's phase at now.
func (h Hint) Phase(now time.Time) Phase {
	switch {
	case now.Before(h.CountsFrom):
		return PhasePending
	case now.Before(h.Expires):
		return PhaseActive
	}
	return PhaseExpired
}

// Claim is one SandboxClaim on the pool.
type Claim struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Ready   bool      `json:"ready"`
}

// Collect reads the pool, its policy, hints and recent claims.
func Collect(ctx context.Context, c client.Reader, namespace, pool string, now time.Time) (*Snapshot, error) {
	s := &Snapshot{At: now, Namespace: namespace, Pool: pool, Hints: []Hint{}, Claims: []Claim{}}

	wp := &extv1beta1.SandboxWarmPool{}
	switch err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pool}, wp); {
	case err == nil:
		s.PoolFound = true
		s.Spec = ptr.Deref(wp.Spec.Replicas, defaultPoolReplicas)
		s.Replicas, s.Ready = wp.Status.Replicas, wp.Status.ReadyReplicas
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get SandboxWarmPool: %w", err)
	}

	policies := &v1alpha1.LookaheadPolicyList{}
	if err := c.List(ctx, policies, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list LookaheadPolicies: %w", err)
	}
	if p := owner(policies.Items, pool); p != nil {
		s.Policy = policyView(p)
	}
	lead := time.Duration(defaultLeadSeconds) * time.Second
	if s.Policy != nil {
		lead = time.Duration(s.Policy.LeadSeconds) * time.Second
	}

	hints := &v1alpha1.DemandHintList{}
	if err := c.List(ctx, hints, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list DemandHints: %w", err)
	}
	for i := range hints.Items {
		h := &hints.Items[i]
		if h.Spec.PoolRef.Name != pool || !h.DeletionTimestamp.IsZero() {
			continue
		}
		s.Hints = append(s.Hints, Hint{Name: h.Name, Replicas: h.Spec.Replicas,
			CountsFrom: h.NeededTime().Add(-lead), NeededAt: h.NeededTime(), Expires: h.ExpiryTime()})
	}
	sort.Slice(s.Hints, func(i, j int) bool { return s.Hints[i].NeededAt.Before(s.Hints[j].NeededAt) })

	claims := &extv1beta1.SandboxClaimList{}
	if err := c.List(ctx, claims, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list SandboxClaims: %w", err)
	}
	for i := range claims.Items {
		cl := &claims.Items[i]
		created := cl.CreationTimestamp.Time
		if cl.Spec.WarmPoolRef.Name != pool || created.Before(now.Add(-ClaimWindow)) {
			continue
		}
		s.Claims = append(s.Claims, Claim{Name: cl.Name, Created: created,
			Ready: meta.IsStatusConditionTrue(cl.Status.Conditions, "Ready")})
	}
	sort.Slice(s.Claims, func(i, j int) bool { return s.Claims[i].Created.Before(s.Claims[j].Created) })
	return s, nil
}

// owner returns the policy that sizes pool: the oldest one targeting it, as the controller decides.
func owner(policies []v1alpha1.LookaheadPolicy, pool string) *v1alpha1.LookaheadPolicy {
	var best *v1alpha1.LookaheadPolicy
	for i := range policies {
		p := &policies[i]
		if p.Spec.PoolRef.Name != pool || !p.DeletionTimestamp.IsZero() {
			continue
		}
		if best == nil || p.CreationTimestamp.Before(&best.CreationTimestamp) ||
			(p.CreationTimestamp.Equal(&best.CreationTimestamp) && strings.Compare(p.Name, best.Name) < 0) {
			best = p
		}
	}
	return best
}

func policyView(p *v1alpha1.LookaheadPolicy) *Policy {
	v := &Policy{
		Name:                 p.Name,
		Min:                  ptr.Deref(p.Spec.MinReplicas, 0),
		Max:                  p.Spec.MaxReplicas,
		LeadSeconds:          ptr.Deref(p.Spec.LeadTimeSeconds, defaultLeadSeconds),
		StabilizationSeconds: ptr.Deref(p.Spec.ScaleDownStabilizationSeconds, defaultStabilizationSeconds),
		Model:                string(p.Spec.Forecast.Model),
		Forecast:             p.Status.ForecastReplicas,
		HintReplicas:         p.Status.HintReplicas,
		ActiveHints:          p.Status.ActiveHints,
		Desired:              p.Status.DesiredReplicas,
	}
	if v.Model == "" {
		v.Model = string(v1alpha1.ForecastModelHolt)
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		v.Reconciled, v.Ready, v.Reason, v.Message = true, c.Status == "True", c.Reason, c.Message
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionScaleTargetConflict); c != nil && c.Status == "True" {
		v.Conflict = c.Message
	}
	if p.Status.LastScaleTime != nil {
		v.LastScale = &p.Status.LastScaleTime.Time
	}
	return v
}

// Pools lists the pools worth showing in namespace: those a LookaheadPolicy targets, or every
// SandboxWarmPool when there are no policies.
func Pools(ctx context.Context, c client.Reader, namespace string) ([]string, error) {
	policies := &v1alpha1.LookaheadPolicyList{}
	if err := c.List(ctx, policies, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list LookaheadPolicies: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range policies.Items {
		seen[p.Spec.PoolRef.Name] = true
	}
	if len(seen) == 0 {
		pools := &extv1beta1.SandboxWarmPoolList{}
		if err := c.List(ctx, pools, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("list SandboxWarmPools: %w", err)
		}
		for _, p := range pools.Items {
			seen[p.Name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
