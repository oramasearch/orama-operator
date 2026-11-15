# e2e-runner

The image the `OramaCloudV1E2ETest` Job runs by default
([#1158](https://github.com/oramasearch/orama-operator/issues/1158), the residual of LSR-216,
[#811](https://github.com/oramasearch/orama-operator/issues/811)): alpine with `curl` and `jq`
already installed. Before it, the Job ran `apk add curl jq` on every start, so a package fetched at
run time, different with each run, ran inside a pod that holds the target stack's master key.

The suite itself is not in the image. The operator embeds `internal/controller/assets/e2e-*.sh` and
mounts them at `/etc/e2e`, so changing the suite never needs a new image. Only a change to the
packages or the alpine base does.

## Status

**Not built yet.** The operator wiring is in place: `e2eRunnerPrebuiltImage` in
`internal/controller/oramacloudv1e2etest_controller.go` is empty, and while it is empty the Job keeps
today's behaviour (the pinned alpine, `apk add` at start). Setting it to the pushed image by digest is
the whole switch: the Job then runs that image and installs nothing.

## Building it (Steve's go-ahead needed for the new repository)

1. Create the private Docker Hub repository **before the first push** (a first push creates it
   public): `POST https://hub.docker.com/v2/repositories/` with
   `{"namespace": "oramasearch", "name": "e2e-runner", "is_private": true}`.
2. Build and push on the cluster BuildKit, from this directory, with the same `buildctl` settings as
   the operator image ([`docs/development.md`](../../docs/development.md)), and record the digest:

   ```sh
   buildctl --addr "$BUILDKIT_HOST" --tlsdir "$BUILDKIT_TLS_DIR" build \
     --frontend dockerfile.v0 \
     --local context=hack/e2e-runner --local dockerfile=hack/e2e-runner \
     --opt platform=linux/amd64 \
     --output "type=image,name=docker.io/oramasearch/e2e-runner:alpine3.20-1,push=true"
   ```

3. Check the repository is still private: an anonymous
   `curl -s -o /dev/null -w '%{http_code}' https://hub.docker.com/v2/repositories/oramasearch/e2e-runner/`
   answers `404`.
4. Set `e2eRunnerPrebuiltImage = "docker.io/oramasearch/e2e-runner:alpine3.20-1@sha256:<digest>"`,
   in a pull request into `dev`. `TestE2ERunnerPrebuiltImageRunsTheSuiteWithoutInstalling` covers the
   switch.

The Job pulls it with the test's `spec.pullSecretName`, which must be able to read the private
repository.
