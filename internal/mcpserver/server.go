// Package mcpserver exposes lookahead to coding agents (Claude Code, Codex and any other MCP
// client) as a stdio MCP server: send DemandHints and read back what the controller did with them.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/hints"
	"github.com/avanishkasar/lookahead/internal/inspect"
)

const instructions = `lookahead sizes agent-sandbox SandboxWarmPools ahead of demand.
Before you create many SandboxClaims from one pool at once (a batch of evals, an RL step, a fan-out),
call create_hint with the pool, how many sandboxes you will claim and how soon, ideally at least the
pool's lead time (usually 30 s) ahead. The pool is then warm when your claims arrive.
Use pool_status to see what lookahead is doing and why; show the user the card it returns.
Delete a hint with delete_hint if the work is cancelled.`

// Config configures the server.
type Config struct {
	// Namespace is used when a tool call names none.
	Namespace string
	// Version is reported to clients.
	Version string
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// PickupTimeout is how long create_hint waits for the controller to count an active hint.
	// Zero skips the wait.
	PickupTimeout time.Duration
}

type server struct {
	c   client.Client
	cfg Config
}

// New returns an MCP server backed by c.
func New(c client.Client, cfg Config) *mcp.Server {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &server{c: c, cfg: cfg}
	srv := mcp.NewServer(&mcp.Implementation{Name: "lookahead", Title: "lookahead", Version: cfg.Version},
		&mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "pool_status",
		Description: "Show what lookahead is doing to a SandboxWarmPool: ready sandboxes, the target and its breakdown (forecast + hints), active and pending hints, and recent claims. Omit pool to show every pool lookahead manages in the namespace.",
		Annotations: &mcp.ToolAnnotations{Title: "Pool status", ReadOnlyHint: true},
	}, s.poolStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_hint",
		Description: "Announce upcoming demand: create a DemandHint saying replicas sandboxes will be claimed from pool in_seconds from now. lookahead grows the pool one lead time before that and keeps it sized until ttl_seconds after.",
		Annotations: &mcp.ToolAnnotations{Title: "Create demand hint", DestructiveHint: new(false)},
	}, s.createHint)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_hints",
		Description: "List DemandHints in the namespace with their phase (pending, active, expired) and timing.",
		Annotations: &mcp.ToolAnnotations{Title: "List demand hints", ReadOnlyHint: true},
	}, s.listHints)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_hint",
		Description: "Delete a DemandHint, for example when the announced work is cancelled. The pool shrinks after the policy's scale-down window.",
		Annotations: &mcp.ToolAnnotations{Title: "Delete demand hint", DestructiveHint: new(true), IdempotentHint: true},
	}, s.deleteHint)
	return srv
}

func (s *server) ns(n string) string {
	if n == "" {
		return s.cfg.Namespace
	}
	return n
}

func text(t string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: t}}}
}

// card renders a snapshot as a brief plus a fenced text card, so markdown clients keep alignment.
func card(snap *inspect.Snapshot) (string, error) {
	var b strings.Builder
	b.WriteString(inspect.Brief(snap) + "\n\n```text\n")
	if err := inspect.Card(&b, snap, nil, inspect.Style{}); err != nil {
		return "", err
	}
	b.WriteString("```\n")
	return b.String(), nil
}

// PoolStatusInput selects the pools to show.
type PoolStatusInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace of the pool; defaults to the server's namespace"`
	Pool      string `json:"pool,omitempty" jsonschema:"SandboxWarmPool name; omit for every pool lookahead manages in the namespace"`
}

func (s *server) poolStatus(ctx context.Context, _ *mcp.CallToolRequest, in PoolStatusInput) (*mcp.CallToolResult, any, error) {
	ns := s.ns(in.Namespace)
	pools := []string{in.Pool}
	if in.Pool == "" {
		var err error
		if pools, err = inspect.Pools(ctx, s.c, ns); err != nil {
			return nil, nil, err
		}
		if len(pools) == 0 {
			return text(fmt.Sprintf("No SandboxWarmPools in namespace %s.", ns)), nil, nil
		}
	}
	var parts []string
	for _, p := range pools {
		snap, err := inspect.Collect(ctx, s.c, ns, p, s.cfg.Now())
		if err != nil {
			return nil, nil, err
		}
		t, err := card(snap)
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, t)
	}
	return text(strings.Join(parts, "\n")), nil, nil
}

// CreateHintInput is a DemandHint to create.
type CreateHintInput struct {
	Namespace  string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace of the pool; defaults to the server's namespace"`
	Pool       string `json:"pool" jsonschema:"SandboxWarmPool the sandboxes will be claimed from"`
	Replicas   int    `json:"replicas" jsonschema:"how many sandboxes will be claimed (1-10000)"`
	InSeconds  int    `json:"in_seconds,omitempty" jsonschema:"seconds from now when the sandboxes are needed; 0 means now"`
	TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"seconds after the needed time that the hint keeps counting; default 300"`
	Name       string `json:"name,omitempty" jsonschema:"hint name; generated from the pool name if omitted"`
}

func (s *server) createHint(ctx context.Context, _ *mcp.CallToolRequest, in CreateHintInput) (*mcp.CallToolResult, any, error) {
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if in.TTLSeconds == 0 {
		ttl = time.Duration(v1alpha1.DefaultHintTTLSeconds) * time.Second
	}
	req := hints.Request{Namespace: s.ns(in.Namespace), Pool: in.Pool, Name: in.Name, Replicas: in.Replicas,
		In: time.Duration(in.InSeconds) * time.Second, TTL: ttl}
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}

	now := s.cfg.Now()
	before, err := inspect.Collect(ctx, s.c, req.Namespace, req.Pool, now)
	if err != nil {
		return nil, nil, err
	}
	if !before.PoolFound {
		return nil, nil, fmt.Errorf("SandboxWarmPool %s/%s not found", req.Namespace, req.Pool)
	}
	h := req.Object(now)
	if err := s.c.Create(ctx, h); err != nil {
		return nil, nil, fmt.Errorf("create DemandHint: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Created demandhint/%s: +%d sandboxes for %s, needed at %s, counting until %s.\n",
		h.Name, req.Replicas, req.Pool, h.NeededTime().Format("15:04:05"), h.ExpiryTime().Format("15:04:05"))
	p := before.Policy
	switch {
	case p == nil:
		b.WriteString("Warning: no LookaheadPolicy targets this pool, so the hint has no effect until one does.\n")
	case p.Conflict != "":
		fmt.Fprintf(&b, "Warning: lookahead is not scaling this pool (%s), so the hint has no effect.\n", p.Conflict)
	default:
		lead := time.Duration(p.LeadSeconds) * time.Second
		if counts := h.NeededTime().Add(-lead); counts.After(now) {
			fmt.Fprintf(&b, "lookahead starts counting it in %s (%d s lead before the needed time).\n",
				inspect.Short(counts.Sub(now)), p.LeadSeconds)
		} else if after, waited, ok := s.awaitPickup(ctx, req, p.ActiveHints); ok {
			fmt.Fprintf(&b, "lookahead counted it after %s: target %d → %d.\n", waited.Round(100*time.Millisecond), p.Desired, after.Policy.Desired)
		} else if s.cfg.PickupTimeout > 0 {
			fmt.Fprintf(&b, "The controller has not counted it yet after %s; check pool_status shortly.\n", inspect.Short(s.cfg.PickupTimeout))
		}
	}

	snap, err := inspect.Collect(ctx, s.c, req.Namespace, req.Pool, s.cfg.Now())
	if err != nil {
		return nil, nil, err
	}
	t, err := card(snap)
	if err != nil {
		return nil, nil, err
	}
	b.WriteString("\n" + t)
	return text(b.String()), nil, nil
}

// awaitPickup polls until the policy reports more active hints than before.
func (s *server) awaitPickup(ctx context.Context, req hints.Request, activeBefore int32) (*inspect.Snapshot, time.Duration, bool) {
	if s.cfg.PickupTimeout <= 0 {
		return nil, 0, false
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.PickupTimeout)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, 0, false
		case <-tick.C:
			snap, err := inspect.Collect(ctx, s.c, req.Namespace, req.Pool, s.cfg.Now())
			if err == nil && snap.Policy != nil && snap.Policy.ActiveHints > activeBefore {
				return snap, time.Since(start), true
			}
		}
	}
}

// ListHintsInput selects hints to list.
type ListHintsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace; defaults to the server's namespace"`
	Pool      string `json:"pool,omitempty" jsonschema:"only hints for this SandboxWarmPool"`
}

func (s *server) listHints(ctx context.Context, _ *mcp.CallToolRequest, in ListHintsInput) (*mcp.CallToolResult, any, error) {
	ns := s.ns(in.Namespace)
	list := &v1alpha1.DemandHintList{}
	if err := s.c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("list DemandHints: %w", err)
	}
	policies := &v1alpha1.LookaheadPolicyList{}
	if err := s.c.List(ctx, policies, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("list LookaheadPolicies: %w", err)
	}
	lead := map[string]time.Duration{}
	for _, p := range policies.Items {
		sec := int32(30)
		if p.Spec.LeadTimeSeconds != nil {
			sec = *p.Spec.LeadTimeSeconds
		}
		lead[p.Spec.PoolRef.Name] = time.Duration(sec) * time.Second
	}

	now := s.cfg.Now()
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPOOL\tREPLICAS\tPHASE\tNEEDED\tEXPIRES")
	n := 0
	for i := range list.Items {
		h := &list.Items[i]
		if in.Pool != "" && h.Spec.PoolRef.Name != in.Pool {
			continue
		}
		l, ok := lead[h.Spec.PoolRef.Name]
		if !ok {
			l = 30 * time.Second
		}
		v := inspect.Hint{Name: h.Name, Replicas: h.Spec.Replicas, CountsFrom: h.NeededTime().Add(-l),
			NeededAt: h.NeededTime(), Expires: h.ExpiryTime()}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", h.Name, h.Spec.PoolRef.Name, h.Spec.Replicas, v.Phase(now),
			relative(v.NeededAt, now), relative(v.Expires, now))
		n++
	}
	if n == 0 {
		return text(fmt.Sprintf("No DemandHints in namespace %s.", ns)), nil, nil
	}
	if err := tw.Flush(); err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("%d DemandHint(s) in %s:\n\n```text\n%s```\n", n, ns, b.String())), nil, nil
}

func relative(t, now time.Time) string {
	if t.After(now) {
		return "in " + inspect.Short(t.Sub(now))
	}
	return inspect.Short(now.Sub(t)) + " ago"
}

// DeleteHintInput names the hint to delete.
type DeleteHintInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace; defaults to the server's namespace"`
	Name      string `json:"name" jsonschema:"DemandHint name"`
}

func (s *server) deleteHint(ctx context.Context, _ *mcp.CallToolRequest, in DeleteHintInput) (*mcp.CallToolResult, any, error) {
	if in.Name == "" {
		return nil, nil, errors.New("name is required")
	}
	h := &v1alpha1.DemandHint{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns(in.Namespace), Name: in.Name}}
	if err := s.c.Delete(ctx, h); err != nil {
		return nil, nil, fmt.Errorf("delete DemandHint %s: %w", in.Name, err)
	}
	return text(fmt.Sprintf("Deleted demandhint/%s. Its pool shrinks once the policy's scale-down window passes.", in.Name)), nil, nil
}
