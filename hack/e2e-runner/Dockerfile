# The `OramaCloudV1E2ETest` runner (operator#1158): alpine with curl and jq already installed, so the
# Job never fetches a package at start inside a pod that holds the target's master key. It runs the
# suite the operator mounts at /etc/e2e (assets/e2e-functional.sh and the files it sources); nothing
# of the suite is baked in, so a suite change never needs a new image.
#
# Built on the cluster BuildKit and pushed to the private Docker Hub repository
# oramasearch/e2e-runner, which is created private before the first push (see README.md). Pass the
# base image by digest. The packages come from that alpine release's repository at build time; the
# image digest the operator pins (e2eRunnerPrebuiltImage) fixes the result.
ARG ALPINE_IMAGE=alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
FROM ${ALPINE_IMAGE}
RUN apk add --no-cache curl jq \
 && curl --version >/dev/null \
 && jq --version >/dev/null
# The suite needs no privilege: it only calls the stack's Services over HTTP.
USER 65534:65534
WORKDIR /tmp
ENTRYPOINT []
CMD ["sh"]
