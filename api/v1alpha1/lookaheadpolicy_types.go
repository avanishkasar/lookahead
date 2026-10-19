package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ForecastModel selects the arrival forecaster.
// +kubebuilder:validation:Enum=None;EWMA;Holt
type ForecastModel string

const (
	// ForecastModelNone disables the forecast; the pool is sized from hints and minReplicas only.
	ForecastModelNone ForecastModel = "None"
	// ForecastModelEWMA is single exponential smoothing: a level, no trend.
	ForecastModelEWMA ForecastModel = "EWMA"
	// ForecastModelHolt is Holt's linear (double exponential) smoothing: a level and a trend.
	ForecastModelHolt ForecastModel = "Holt"
)

// ForecastSpec configures the claim arrival forecaster. Smoothing factors are percentages
// because the Kubernetes API conventions discourage floating point fields.
type ForecastSpec struct {
	// model is the forecaster. Defaults to Holt.
	// +optional
	// +kubebuilder:default=Holt
	Model ForecastModel `json:"model,omitempty"`

	// alphaPercent is the level smoothing factor in percent (1-100). Higher reacts faster.
	// +optional
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	AlphaPercent *int32 `json:"alphaPercent,omitempty"`

	// betaPercent is the trend smoothing factor in percent (0-100). Ignored for EWMA.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	BetaPercent *int32 `json:"betaPercent,omitempty"`

	// safetyFactorPercent scales the Poisson headroom term z*sqrt(expected), with z in percent.
	// 0 disables headroom.
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	SafetyFactorPercent *int32 `json:"safetyFactorPercent,omitempty"`

	// bucketSeconds is the width of one arrival-count bucket.
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	BucketSeconds *int32 `json:"bucketSeconds,omitempty"`

	// historySeconds is how much arrival history the forecaster fits on.
	// +optional
	// +kubebuilder:default=1800
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=3600
	HistorySeconds *int32 `json:"historySeconds,omitempty"`
}

// LookaheadPolicySpec defines predictive sizing for one SandboxWarmPool.
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must be less than or equal to maxReplicas"
type LookaheadPolicySpec struct {
	// poolRef is the SandboxWarmPool in this namespace whose spec.replicas this policy sets.
	// Immutable: a policy that should target another pool is a new policy.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolRef is immutable"
	PoolRef PoolReference `json:"poolRef"`

	// minReplicas is the lower bound of the pool size.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// maxReplicas is the upper bound of the pool size, including hints.
	// +required
	// +kubebuilder:validation:Minimum=0
	MaxReplicas int32 `json:"maxReplicas"`

	// leadTimeSeconds is how far ahead the pool is sized: the time a new sandbox needs to become
	// Ready. Hints start counting this long before their neededAt.
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	LeadTimeSeconds *int32 `json:"leadTimeSeconds,omitempty"`

	// scaleDownStabilizationSeconds is the window whose highest recommendation is applied, so
	// the pool grows immediately and shrinks only after demand has stayed low this long.
	// +optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	ScaleDownStabilizationSeconds *int32 `json:"scaleDownStabilizationSeconds,omitempty"`

	// forecast configures the arrival forecaster.
	// +optional
	Forecast ForecastSpec `json:"forecast,omitempty"`
}

// Condition types for LookaheadPolicy.
const (
	// ConditionReady is True when the policy is sizing its pool.
	ConditionReady = "Ready"
	// ConditionScaleTargetConflict is True when another owner of the pool's replicas exists
	// (an HPA, a KEDA ScaledObject's HPA, or an older LookaheadPolicy). The policy does not write
	// spec.replicas while this is True.
	ConditionScaleTargetConflict = "ScaleTargetConflict"
)

// Condition reasons for LookaheadPolicy.
const (
	ReasonScaling            = "Scaling"
	ReasonPoolNotFound       = "PoolNotFound"
	ReasonConflict           = "ScaleTargetConflict"
	ReasonNoConflict         = "NoConflict"
	ReasonHPATargetsPool     = "HorizontalPodAutoscalerTargetsPool"
	ReasonOlderPolicyExists  = "OlderPolicyTargetsPool"
	ReasonScaleUpdateFailure = "ScaleUpdateFailed"
)

// LookaheadPolicyStatus is the observed state of a LookaheadPolicy.
type LookaheadPolicyStatus struct {
	// conditions describe the policy's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// forecastReplicas is the forecast part of the last recommendation.
	// +optional
	ForecastReplicas int32 `json:"forecastReplicas,omitempty"`

	// hintReplicas is the sum of replicas of active DemandHints at the last recommendation.
	// +optional
	HintReplicas int32 `json:"hintReplicas,omitempty"`

	// activeHints is the number of DemandHints active at the last recommendation.
	// +optional
	ActiveHints int32 `json:"activeHints,omitempty"`

	// desiredReplicas is the stabilized, clamped target last computed for the pool.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// lastScaleTime is when this policy last changed the pool's spec.replicas.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=lap
// +kubebuilder:printcolumn:name="Pool",type="string",JSONPath=".spec.poolRef.name"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desiredReplicas"
// +kubebuilder:printcolumn:name="Forecast",type="integer",JSONPath=".status.forecastReplicas"
// +kubebuilder:printcolumn:name="Hints",type="integer",JSONPath=".status.hintReplicas"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LookaheadPolicy sizes a SandboxWarmPool ahead of demand from a claim forecast and DemandHints.
type LookaheadPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec LookaheadPolicySpec `json:"spec"`

	// +optional
	Status LookaheadPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LookaheadPolicyList contains a list of LookaheadPolicy.
type LookaheadPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LookaheadPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &LookaheadPolicy{}, &LookaheadPolicyList{})
		return nil
	})
}
