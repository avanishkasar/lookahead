// Package v1alpha1 contains the prototype API for lookahead.
//
// This group is a prototype only. The upstream shape (fields on SandboxWarmPool or an object in
// extensions.agents.x-k8s.io) is decided with agent-sandbox maintainers; see docs/keps.
// +kubebuilder:object:generate=true
// +groupName=lookahead.avanishkasar.github.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "lookahead.avanishkasar.github.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
