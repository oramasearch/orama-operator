# A scoped credential for build base images, and rotating the platform pull token

> **Human action: Steve. Open.** The operator change is done (orama-operator#758). A person must create a
> read-only Docker Hub token and the Secret that holds it, then rotate the platform pull token that
> builders could read before the change.

## Symptom

Once the operator includes #758, a build whose `Dockerfile` starts `FROM` a private `oramasearch/*`
image fails to pull its base image, for example the app runtime (`APP_RUNTIME_IMAGE`) or the methodology
runtime (`METHODOLOGY_RUNTIME_IMAGE`). The builder no longer receives the Lake's `pullSecretName`.

## Why

Before #758, every builder pod mounted the Lake's `pullSecretName` (`oramasearch-dockerhub`), and
lake-builder merged it into BuildKit's Docker configuration. Tenant source runs in that pod, with
BuildKit's process sandbox off, so any build step could read the credential and send it out. That
token pulls every private Orama image, and may also push.

## Read-only check

```sh
# Which Lakes name a base-image credential (empty: none yet)
kubectl --context <cluster> get lakes -o custom-columns=NAME:.metadata.name,BASE:.spec.builds.baseImagePullSecretName
# Is a builder pod mounting anything but builder-base-pull? (during a build)
kubectl --context <cluster> get pods -A -l lake.orama.cloud/build -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.spec.volumes[*].secret.secretName}{"\n"}{end}'
```

## Steps

1. In Docker Hub, create a **read-only** personal access token, or a service account token, with access
   to only the base repositories builds start from (today `oramasearch/app-runtime` and
   `oramasearch/methodology-runtime`). Docker Hub scopes an organization access token per repository.
2. Store it as a dockerconfigjson Secret in each Lake's namespace. This is one of the source Secrets
   the sets copy, like the pull secret, so add it the way those are added (the `lake` namespace on the
   managing cluster). Name it `oramasearch-dockerhub-build-base`.
3. Set `spec.builds.baseImagePullSecretName: oramasearch-dockerhub-build-base` in the set files (in
   `orama-infra`'s `config/clusters/sets/`), commit, and apply with consent: dev first, per the
   pipeline.
4. **Rotate `oramasearch-dockerhub`**: create a new token, update the Secret where it lives, and
   revoke the old token. Builds have run with the old token mounted, so treat it as exposed.

## Confirm

- A build `FROM` the app runtime succeeds on `lake-dev`.
- During a build, the builder pod's Secret volumes list only `builder-base-pull`, and
  `/etc/lake-builder/pull/.dockerconfigjson` holds only the scoped token's registry entry.
- The old `oramasearch-dockerhub` token is shown as revoked in Docker Hub.
