# CRDs

The CRD YAML files are **generated** from the kubebuilder markers in
`../../api/v1alpha1/*.go` via `make manifests` (needs Go + controller-gen), then
copied here by `make helm-package`. Helm applies anything in this `crds/` directory
before the templates. Until codegen runs, install the CRD out-of-band or run
`make helm-package`.
