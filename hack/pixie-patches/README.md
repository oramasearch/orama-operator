# Patches for oramasearch/pixie (#455)

Source changes the operator's Pixie install needs and stock Pixie does not have. They are carried on
the `orama` branch of `oramasearch/pixie`, on top of the commit the operator vendors
(`internal/controller/assets/pixie/COMMIT`), and the images of #455 are built from that branch. Each
applies with `git apply` at that commit (checked with `git apply --check`).

| Patch | Why |
|---|---|
| `0001-api-script-bundle-urls.patch` | The API server reads PxL scripts for autocomplete from two public URLs written into its source (`storage.googleapis.com`, `artifacts.px.dev`), refreshed every 30 seconds, with no flag. With the patch it reads `SCRIPT_BUNDLE_URLS`, which the operator already sets to the hub's own `/pxl_scripts/bundle.json`. Without it an air-gapped hub works, but its autocomplete has no scripts, and the fetches fail closed (the hub's sandbox allows no internet egress). |
| `0002-k8s-name-every-image-pixie.patch` | Names every image `pixie-*` (`$(IMAGE_PREFIX)/pixie-vizier-pem_image`), so Pixie's images sit beside Orama's in `oramasearch` on Docker Hub, read as Pixie's, and `operator-operator_image` cannot collide with Orama's operator. |
| `0003-licenses-record-the-Go-modules-pkg.go.dev-rate-limit.patch` | Adds 143 Go modules to `tools/licenses/manual_licenses.json`. A stamped build fails on any module whose licence it cannot fetch, and pkg.go.dev rate-limits the fetch, so each run missed a different 28 to 35 modules. With the licences recorded the build does not depend on pkg.go.dev. |
| `0004-ui-quote-the-stamp-values-the-webpack-bundle-exports.patch` | The UI bundle exports Bazel's stamp values into its shell unquoted. Bazel 7's `FORMATTED_DATE` holds spaces, so a stamped build stopped with `export: '31': not a valid identifier`. The patch quotes each value. |
| `0005-stirling-attach-uprobes-once-per-file.patch` | Stirling attached the 9 OpenSSL uprobes again for every new process that mapped a `libssl` file and never detached them, so the Postgres of the Lake control databases, which forks a backend per connection, climbed to about one core within two days ([#1140](https://github.com/oramasearch/orama-operator/issues/1140), [pixie#1](https://github.com/oramasearch/pixie/issues/1)). With the patch the OpenSSL, Go and gRPC-C uprobes attach once per file (device and inode), every new process still gets its per-process map entries, and a set that fails partway is detached. Not yet built or tested with Bazel. |

`0001` and `0005` change what Pixie does; `0002` to `0004` change only how it is built and named.

## Building (#455)

The release the `ObservabilityProvider` names is built from one commit, the one the operator vendors,
with these patches on top. The build runs in Pixie's own dev image on an x86_64 host with Docker
(`ghcr.io/pixie-io/dev_image_with_extras:202604270358`, the source mounted at
`/px/src/px.dev/pixie`), and it needs a GitHub token in `GH_API_KEY` for the licence fetch. The
`VERSION` and `GIT_COMMIT` files are what the stamp writes into every binary, as Pixie's
`ci/save_version_info.sh` does; without them each binary reports `0.0.0-dev`.

```sh
git -C oramasearch/pixie checkout -b orama 4b445e0b7907c79d034bcb4a227bbaede94d370a
git -C oramasearch/pixie am --3way <this repository>/hack/pixie-patches/*.patch
# Inside the dev image, at /px/src/px.dev/pixie:
flags=(-c opt --config=stamp --config=x86_64_sysroot --//k8s:image_repository=docker.io/oramasearch)
git rev-parse HEAD > GIT_COMMIT
echo <vizier version> > VERSION
bazel run "${flags[@]}" --//k8s:image_version=<vizier version> //k8s/vizier:vizier_images_push
echo <cloud version> > VERSION
bazel run "${flags[@]}" --//k8s:image_version=<cloud version> //k8s/cloud:cloud_images_push
# The script bundle, with the px CLI of the same commit (an absolute path: make's check runs `which`):
echo <cli version> > VERSION
bazel build "${flags[@]}" //src/pixie_cli:px
cp "$(bazel cquery "${flags[@]}" --output=files //src/pixie_cli:px)" /out/px
(cd src/pxl_scripts && make bundle-oss.json EXECUTABLES=/out/px)
cp src/pxl_scripts/bundle-oss.json <this repository>/hack/pixie-script-bundle/
docker buildx build --platform linux/amd64 --provenance=false \
  --build-arg NGINX_IMAGE=docker.io/oramasearch/platform-nginx-unprivileged@sha256:<digest> \
  -t docker.io/oramasearch/pixie-script-bundle:<cloud version> --push <this repository>/hack/pixie-script-bundle
```

Create every Docker Hub repository private before its first push; a first push creates it public.
The versions follow upstream's `-pre-<branch>.N` naming: the release in
`config/clusters/observability-provider.yaml` is Vizier `0.14.16-pre-orama.1`, Cloud
`0.1.10-pre-orama.1` and the CLI `0.8.9-pre-orama.1`.

Then mirror the dependency images of `hack/observability-images.yaml` (`nats`, `curl`, `envoy`,
`hydra`, `hydra-cli`, `kratos`, `postgres`, `opensearch`) by digest, and write every digest into the
`ObservabilityProvider` (`config/clusters/observability-provider.yaml`).
`hack/fetch-pixie-manifests.sh` refuses a commit to which these patches no longer apply.
