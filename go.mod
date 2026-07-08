module github.com/oramasearch/orama-cloud-v1-operator

go 1.23

// Dependency versions are resolved on first `make build` / `go mod tidy`
// (Go is not required to read the API design; it is required to build/codegen).
require (
	k8s.io/api v0.30.3
	k8s.io/apimachinery v0.30.3
	k8s.io/client-go v0.30.3
	sigs.k8s.io/controller-runtime v0.18.4
)
