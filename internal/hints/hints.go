// Package hints builds DemandHints from a request relative to now, shared by the CLI and the MCP
// server so both create identical objects.
package hints

import (
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

// Bounds on a request; they match the DemandHint CRD validation.
const (
	MaxReplicas = 10000
	MinTTL      = time.Second
	MaxTTL      = 24 * time.Hour
)

// Request is a hint to create: replicas sandboxes from pool, needed In from now, counting for TTL
// after that.
type Request struct {
	Namespace string
	Pool      string
	// Name is the hint name; empty generates one from the pool name.
	Name     string
	Replicas int
	In       time.Duration
	TTL      time.Duration
}

// Validate reports the first problem with r, or nil.
func (r Request) Validate() error {
	switch {
	case r.Pool == "":
		return errors.New("pool is required")
	case r.Replicas < 1 || r.Replicas > MaxReplicas:
		return errors.New("replicas must be between 1 and 10000")
	case r.In < 0:
		return errors.New("in must not be negative")
	case r.TTL < MinTTL || r.TTL > MaxTTL:
		return errors.New("ttl must be between 1s and 24h")
	}
	return nil
}

// Object returns the DemandHint for r, with neededAt resolved against now.
func (r Request) Object(now time.Time) *v1alpha1.DemandHint {
	h := &v1alpha1.DemandHint{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.Namespace, Name: r.Name},
		Spec: v1alpha1.DemandHintSpec{
			PoolRef:    v1alpha1.PoolReference{Name: r.Pool},
			Replicas:   int32(r.Replicas),
			NeededAt:   &metav1.Time{Time: now.Add(r.In)},
			TTLSeconds: new(int32(r.TTL.Seconds())),
		},
	}
	if r.Name == "" {
		h.GenerateName = r.Pool + "-"
	}
	return h
}
