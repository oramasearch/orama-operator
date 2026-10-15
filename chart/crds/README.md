# CRDs

The CRD YAML files are **generated** from the kubebuilder markers in
`../../api/v1alpha1/*.go` via `make manifests` (needs Go + controller-gen), then
copied here by `make helm-package`. Helm applies anything in this `crds/` directory
before the templates on installation. For upgrades, install changed CRDs through
the approved rollout mechanism before the new controller runs; see
[`CRD rollout`](../../docs/operations/crd-rollout.md). The DNS CRD must persist
`status.ownerComment` before the DNS ownership controller is deployed.
