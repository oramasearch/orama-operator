# Patches for oramasearch/pixie (#455)

Source changes the operator's Pixie install needs and stock Pixie does not have. They are carried on
the `orama` branch of `oramasearch/pixie`, on top of the commit the operator vendors
(`internal/controller/assets/pixie/COMMIT`), and the images of #455 are built from that branch. Each
applies with `git apply` at that commit (checked with `git apply --check`).

| Patch | Why |
|---|---|
| `0001-api-script-bundle-urls.patch` | The API server reads PxL scripts for autocomplete from two public URLs written into its source (`storage.googleapis.com`, `artifacts.px.dev`), refreshed every 30 seconds, with no flag. With the patch it reads `SCRIPT_BUNDLE_URLS`, which the operator already sets to the hub's own `/pxl_scripts/bundle.json`. Without it an air-gapped hub works, but its autocomplete has no scripts, and the fetches fail closed (the hub's sandbox allows no internet egress). |

The image renaming (`pixie-*`) and the build itself are #455's steps; these patches are only the
behaviour.

## Building (#455)

The release the `ObservabilityProvider` names is built from one commit, the one the operator vendors:

```sh
git -C oramasearch/pixie checkout -b orama 4b445e0b7907c79d034bcb4a227bbaede94d370a
git -C oramasearch/pixie am --3way <this repository>/hack/pixie-patches/*.patch   # or git apply + commit
# Vizier and Cloud images (amd64), named pixie-* per #455's step 1:
bazel run //k8s/vizier:vizier_images_push --//k8s:image_repository=docker.io/oramasearch --//k8s:image_version=<version>
bazel run //k8s/cloud:cloud_images_push   --//k8s:image_repository=docker.io/oramasearch --//k8s:image_version=<version>
# The script bundle, with the px CLI of the same commit:
bazel build //src/pixie_cli:px
make -C src/pxl_scripts bundle-oss.json EXECUTABLES=$(bazel cquery --output=files //src/pixie_cli:px)
cp src/pxl_scripts/bundle-oss.json <this repository>/hack/pixie-script-bundle/
docker build --build-arg NGINX_IMAGE=docker.io/oramasearch/platform-nginx-unprivileged@sha256:<digest> \
  -t docker.io/oramasearch/pixie-script-bundle:<version> <this repository>/hack/pixie-script-bundle
```

Then mirror the dependency images of `hack/observability-images.yaml` (`nats`, `curl`, `envoy`,
`hydra`, `hydra-cli`, `kratos`, `postgres`, `opensearch`), record every digest, and write them into
the `ObservabilityProvider`. `hack/fetch-pixie-manifests.sh` refuses a commit to which these patches
no longer apply.
