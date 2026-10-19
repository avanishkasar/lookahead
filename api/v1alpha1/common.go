package v1alpha1

const (
	// PoolRefField indexes LookaheadPolicies and DemandHints by the SandboxWarmPool they target.
	// It must match the JSON path of PoolRef.Name in both specs.
	PoolRefField = ".spec.poolRef.name"
)

// PoolReference names a SandboxWarmPool in the same namespace.
type PoolReference struct {
	// name of the SandboxWarmPool.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}
