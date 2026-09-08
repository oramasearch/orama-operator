# Installation registry credentials

Follow [the installation guide](../install.md) in order: obtain published immutable images and
registry access, create the namespace and pull Secret, configure the manager values, install the
platform controllers, render and install the manager, verify its startup, then apply the sample.
The operator image is private; the chart and helmfile cannot grant registry access or create an
account on the reader's behalf. Refs [#158](https://github.com/oramasearch/orama-operator/issues/158).

## Which setting configures which pod

| Consumer | Setting | Secret namespace | Who creates the Secret |
|---|---|---|---|
| Manager Deployment | Helm `operator.imagePullSecrets`, a list of `{name: ...}` references | Helm release namespace (`orama-system` in the guide) | Installer, before installing the manager |
| `OramaCloudV1` application pods | CR `spec.images.pullSecretName`, a Secret name | CR namespace | Installer, before applying the CR |
| `Lake` service | CR `spec.pullSecretName`, a Secret name | Lake's `spec.namespace` | Installer, before applying the Lake |

The first two can reference the same Secret because the guide installs both in `orama-system`.
A Lake in another namespace needs its own Secret there. The Lake image also requires registry
access; see [k0s.md](k0s.md). A generated application password or a sealed application secret does
not solve bootstrap access for a manager that cannot yet start.

## Render proof without a cluster

The chart default remains `oramasearch/orama-operator:v1-sandbox` with `IfNotPresent` and no pull
Secret. The install guide requires an accessible immutable tag and provides a values file before
its first manager installation. To compare the chart default with the credential value alone:

```sh
helm template orama-operator chart/ --namespace orama-system \
  | grep -A3 -E "image:|imagePullSecrets"
helm template orama-operator chart/ --namespace orama-system \
  --set 'operator.imagePullSecrets[0].name=oramasearch-dockerhub' \
  | grep -A3 -E "image:|imagePullSecrets"
```

The second rendering adds this Pod-spec fragment without changing the image:

```yaml
imagePullSecrets:
  - name: oramasearch-dockerhub
containers:
  - name: manager
    image: "oramasearch/orama-operator:v1-sandbox"
    imagePullPolicy: IfNotPresent
```

For installation, render with `-f operator-values.yaml` as shown in the main guide so the supplied
immutable tag replaces `v1-sandbox`. The equivalent Helmfile render is:

```sh
helmfile -l app=orama-operator template --values "$PWD/operator-values.yaml"
```

Inspect the `orama-operator` Deployment in the release namespace. The `manager` container must
use the supplied image and `spec.template.spec.imagePullSecrets` must name the existing Secret.
No credential belongs in the values file, rendered Deployment, repository, or review output.
A successful render or `helm lint` does not confirm publication, credentials, node connectivity,
or a running manager; the live rollout and sample checks in the installation guide establish those.

## Diagnose a manager that does not start

Use events and logs, which do not require displaying the Secret's contents:

```sh
kubectl -n orama-system describe deployment orama-operator
kubectl -n orama-system describe pods -l app.kubernetes.io/name=orama-operator
kubectl -n orama-system logs deployment/orama-operator --tail=100
```

| Observation | Check |
|---|---|
| `FailedToRetrieveImagePullSecret` | The referenced Secret exists in the manager's namespace and its name matches the rendered Pod spec. |
| `ErrImagePull` / `ImagePullBackOff`, unauthorized | The registry identity has read access to this exact repository; the Secret's registry server matches the image host. |
| Manifest/tag not found or platform mismatch | Obtain the exact published immutable tag for the node architecture from the release maintainer. Do not assume `latest` or `v1-sandbox` is accessible. |
| Pull timeout / connection failure | Node access to the registry and its authentication endpoints. |
| Container starts, then exits or cache sync fails | Inspect logs, installed CRDs, CloudNativePG availability, and RBAC compatibility with the supplied build. |

The chart has no manager readiness probe. Require a successful rollout, a running manager with
no restarts, and startup logs without errors before applying the sample; then wait independently
for the sample's Ready condition. Do not treat a successful Helm command alone as application
readiness. Remote image publication uses the [BuildKit runbook](../development.md#obtain-or-publish-the-operator-image).
