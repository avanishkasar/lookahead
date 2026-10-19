// Package replay replays a trace against a cluster and measures claim latency, launch type and
// warm pool size.
package replay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/trace"
)

// RunLabel marks every object a replay creates so it can be watched and cleaned up.
const RunLabel = "lookahead.avanishkasar.github.io/run"

// Config configures one replay.
type Config struct {
	Namespace string
	Pool      string
	TraceName string
	Strategy  string
	RunID     string
	// UseHints creates the trace's hint events as DemandHints; otherwise they are skipped.
	UseHints       bool
	ReadyTimeout   time.Duration
	SampleInterval time.Duration
}

type claimState struct {
	ready   chan struct{}
	readyAt time.Time
}

type runner struct {
	c   client.WithWatch
	cfg Config

	mu     sync.Mutex
	claims map[string]*claimState
}

// Run replays events and returns the measurements. Objects it created are deleted before it
// returns, also when ctx is cancelled.
func Run(ctx context.Context, c client.WithWatch, events []trace.Event, cfg Config) (*trace.Result, error) {
	if cfg.ReadyTimeout <= 0 || cfg.SampleInterval <= 0 {
		return nil, errors.New("ready timeout and sample interval must be positive")
	}
	r := &runner{c: c, cfg: cfg, claims: map[string]*claimState{}}
	logger := log.FromContext(ctx)

	if err := c.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.Pool}, &extv1beta1.SandboxWarmPool{}); err != nil {
		return nil, fmt.Errorf("get SandboxWarmPool %s/%s: %w", cfg.Namespace, cfg.Pool, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer r.cleanup(context.WithoutCancel(ctx))

	watchDone := make(chan error, 1)
	go func() { watchDone <- r.watchClaims(runCtx) }()

	res := &trace.Result{Trace: cfg.TraceName, Strategy: cfg.Strategy, RunID: cfg.RunID, StartedAt: time.Now()}
	var samplesMu sync.Mutex
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		r.sample(runCtx, func(s trace.PoolSample) {
			samplesMu.Lock()
			res.Samples = append(res.Samples, s)
			samplesMu.Unlock()
		})
	}()

	var (
		wg       sync.WaitGroup
		resultMu sync.Mutex
	)
	start := res.StartedAt
	for i, ev := range events {
		wait := time.Until(start.Add(time.Duration(ev.AtMs) * time.Millisecond))
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				wg.Wait()
				return nil, ctx.Err()
			}
		}
		switch ev.Kind {
		case trace.KindHint:
			if !cfg.UseHints {
				continue
			}
			if err := r.createHint(ctx, i, ev); err != nil {
				logger.Error(err, "Failed to create DemandHint", "event", i)
			}
		case trace.KindClaim:
			wg.Go(func() {
				cr := r.runClaim(runCtx, i, ev)
				resultMu.Lock()
				res.Claims = append(res.Claims, cr)
				resultMu.Unlock()
			})
		}
	}
	wg.Wait()
	cancel()
	<-samplerDone
	if err := <-watchDone; err != nil && !errors.Is(err, context.Canceled) {
		logger.Error(err, "Claim watch ended with error")
	}
	res.EndedAt = time.Now()
	res.Summary = trace.Summarize(res)
	return res, nil
}

func (r *runner) labels() map[string]string {
	return map[string]string{RunLabel: r.cfg.RunID}
}

func (r *runner) createHint(ctx context.Context, i int, ev trace.Event) error {
	h := &v1alpha1.DemandHint{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.cfg.Namespace, Name: fmt.Sprintf("%s-h%d", r.cfg.RunID, i), Labels: r.labels(),
		},
		Spec: v1alpha1.DemandHintSpec{
			PoolRef:    v1alpha1.PoolReference{Name: r.cfg.Pool},
			Replicas:   ev.Replicas,
			NeededAt:   &metav1.Time{Time: time.Now().Add(time.Duration(ev.NeededInMs) * time.Millisecond)},
			TTLSeconds: new(ev.TTLSeconds),
		},
	}
	return r.c.Create(ctx, h)
}

// runClaim creates one claim, waits for Ready, records its launch type and deletes it after its
// hold time.
func (r *runner) runClaim(ctx context.Context, i int, ev trace.Event) trace.ClaimResult {
	name := fmt.Sprintf("%s-c%d", r.cfg.RunID, i)
	st := &claimState{ready: make(chan struct{})}
	r.mu.Lock()
	r.claims[name] = st
	r.mu.Unlock()

	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.cfg.Namespace, Name: name, Labels: r.labels()},
		Spec:       extv1beta1.SandboxClaimSpec{WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: r.cfg.Pool}},
	}
	cr := trace.ClaimResult{Name: name, CreatedAt: time.Now()}
	if err := r.c.Create(ctx, claim); err != nil {
		cr.Error = fmt.Sprintf("create: %v", err)
		return cr
	}
	deleteAt := cr.CreatedAt.Add(time.Duration(ev.HoldMs) * time.Millisecond)

	select {
	case <-st.ready:
		r.mu.Lock()
		cr.ReadyAt = st.readyAt
		r.mu.Unlock()
		cr.LaunchType = r.launchType(ctx, name)
	case <-time.After(r.cfg.ReadyTimeout):
		cr.Error = "not Ready within timeout"
	case <-ctx.Done():
		cr.Error = ctx.Err().Error()
		return cr
	}

	if wait := time.Until(deleteAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return cr
		}
	}
	if err := r.c.Delete(ctx, claim, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		log.FromContext(ctx).Error(err, "Failed to delete claim", "claim", name)
	}
	return cr
}

// launchType reads the launch-type label of the Sandbox bound to the claim.
func (r *runner) launchType(ctx context.Context, claimName string) string {
	for range 10 {
		claim := &extv1beta1.SandboxClaim{}
		if err := r.c.Get(ctx, types.NamespacedName{Namespace: r.cfg.Namespace, Name: claimName}, claim); err == nil &&
			claim.Status.SandboxStatus.Name != "" {
			sb := &sandboxv1beta1.Sandbox{}
			if err := r.c.Get(ctx, types.NamespacedName{Namespace: r.cfg.Namespace, Name: claim.Status.SandboxStatus.Name}, sb); err == nil {
				if lt := sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; lt != "" {
					return lt
				}
			}
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ""
		}
	}
	return ""
}

// watchClaims marks claims Ready at the moment the watch reports it. It re-establishes the
// watch when the server closes it.
func (r *runner) watchClaims(ctx context.Context) error {
	for {
		w, err := r.c.Watch(ctx, &extv1beta1.SandboxClaimList{},
			client.InNamespace(r.cfg.Namespace), client.MatchingLabels(r.labels()))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.FromContext(ctx).Error(err, "Claim watch failed; retrying")
			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		r.consume(ctx, w)
		w.Stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (r *runner) consume(ctx context.Context, w watch.Interface) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			claim, isClaim := ev.Object.(*extv1beta1.SandboxClaim)
			if !isClaim || (ev.Type != watch.Added && ev.Type != watch.Modified) {
				continue
			}
			if !meta.IsStatusConditionTrue(claim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady)) {
				continue
			}
			now := time.Now()
			r.mu.Lock()
			if st, ok := r.claims[claim.Name]; ok && st.readyAt.IsZero() {
				st.readyAt = now
				close(st.ready)
			}
			r.mu.Unlock()
		}
	}
}

func (r *runner) sample(ctx context.Context, record func(trace.PoolSample)) {
	t := time.NewTicker(r.cfg.SampleInterval)
	defer t.Stop()
	for {
		pool := &extv1beta1.SandboxWarmPool{}
		s := trace.PoolSample{At: time.Now()}
		if err := r.c.Get(ctx, types.NamespacedName{Namespace: r.cfg.Namespace, Name: r.cfg.Pool}, pool); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.ObservationLost = true
		} else {
			s.SpecReplicas = ptr.Deref(pool.Spec.Replicas, 1)
			s.Replicas = pool.Status.Replicas
			s.ReadyReplicas = pool.Status.ReadyReplicas
		}
		record(s)
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func (r *runner) cleanup(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	logger := log.FromContext(ctx)
	opts := []client.DeleteAllOfOption{
		client.InNamespace(r.cfg.Namespace), client.MatchingLabels(r.labels()),
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	}
	if err := r.c.DeleteAllOf(ctx, &extv1beta1.SandboxClaim{}, opts...); err != nil {
		logger.Error(err, "Failed to clean up claims")
	}
	if err := r.c.DeleteAllOf(ctx, &v1alpha1.DemandHint{}, opts...); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to clean up hints")
	}
}
