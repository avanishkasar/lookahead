package lookahead

// Run `go generate ./...` after changing api/ or RBAC markers. It works from PowerShell and
// any Unix shell; there is no Makefile dependency.

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0 object paths=./api/v1alpha1/...
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0 crd paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0 rbac:roleName=lookahead-controller paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
