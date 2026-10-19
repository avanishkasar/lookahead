package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DefaultHintTTLSeconds is the TTL applied when spec.ttlSeconds is unset.
const DefaultHintTTLSeconds int32 = 300

// DemandHintSpec declares that the creator expects to claim replicas sandboxes from a pool.
type DemandHintSpec struct {
	// poolRef is the SandboxWarmPool in this namespace the sandboxes will be claimed from.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolRef is immutable"
	PoolRef PoolReference `json:"poolRef"`

	// replicas is how many sandboxes the creator expects to claim.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	Replicas int32 `json:"replicas"`

	// neededAt is when the sandboxes are needed. The hint starts counting one lead time
	// (LookaheadPolicy.spec.leadTimeSeconds) before it. Defaults to the creation time.
	// +optional
	NeededAt *metav1.Time `json:"neededAt,omitempty"`

	// ttlSeconds is how long after neededAt the hint keeps counting. The controller deletes the
	// hint when it expires, so a crashed creator never keeps a pool inflated.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TTLSeconds *int32 `json:"ttlSeconds,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dh
// +kubebuilder:printcolumn:name="Pool",type="string",JSONPath=".spec.poolRef.name"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="NeededAt",type="date",JSONPath=".spec.neededAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// DemandHint is a short-lived declaration of upcoming SandboxClaims for a pool.
type DemandHint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DemandHintSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// DemandHintList contains a list of DemandHint.
type DemandHintList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemandHint `json:"items"`
}

// NeededTime is spec.neededAt, or the creation time when unset.
func (h *DemandHint) NeededTime() time.Time {
	if h.Spec.NeededAt != nil {
		return h.Spec.NeededAt.Time
	}
	return h.CreationTimestamp.Time
}

// ExpiryTime is when the hint stops counting and becomes eligible for deletion.
func (h *DemandHint) ExpiryTime() time.Time {
	ttl := DefaultHintTTLSeconds
	if h.Spec.TTLSeconds != nil {
		ttl = *h.Spec.TTLSeconds
	}
	return h.NeededTime().Add(time.Duration(ttl) * time.Second)
}

// ActiveAt reports whether the hint counts toward the pool target at now, given the policy's
// lead time.
func (h *DemandHint) ActiveAt(now time.Time, leadTime time.Duration) bool {
	return !now.Before(h.NeededTime().Add(-leadTime)) && now.Before(h.ExpiryTime())
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &DemandHint{}, &DemandHintList{})
		return nil
	})
}
