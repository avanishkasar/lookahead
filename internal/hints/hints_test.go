package hints

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := Request{Pool: "p", Replicas: 5, In: 30 * time.Second, TTL: time.Minute}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	for name, mutate := range map[string]func(*Request){
		"no pool":       func(r *Request) { r.Pool = "" },
		"zero replicas": func(r *Request) { r.Replicas = 0 },
		"too many":      func(r *Request) { r.Replicas = MaxReplicas + 1 },
		"negative in":   func(r *Request) { r.In = -time.Second },
		"short ttl":     func(r *Request) { r.TTL = 0 },
		"long ttl":      func(r *Request) { r.TTL = MaxTTL + time.Second },
	} {
		r := ok
		mutate(&r)
		if r.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestObject(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := Request{Namespace: "ns", Pool: "pool", Replicas: 40, In: 45 * time.Second, TTL: 90 * time.Second}.Object(now)
	if h.Namespace != "ns" || h.Name != "" || h.GenerateName != "pool-" {
		t.Errorf("meta = %s/%q generateName %q", h.Namespace, h.Name, h.GenerateName)
	}
	if h.Spec.PoolRef.Name != "pool" || h.Spec.Replicas != 40 || *h.Spec.TTLSeconds != 90 {
		t.Errorf("spec = %+v", h.Spec)
	}
	if !h.NeededTime().Equal(now.Add(45 * time.Second)) {
		t.Errorf("neededAt = %v", h.NeededTime())
	}
	if named := (Request{Pool: "pool", Name: "rl-step"}).Object(now); named.Name != "rl-step" || named.GenerateName != "" {
		t.Errorf("named hint: name %q generateName %q", named.Name, named.GenerateName)
	}
}
