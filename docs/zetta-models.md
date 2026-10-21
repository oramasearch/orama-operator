# Model checkpoints on a Zetta's data volume (`spec.models`)

An `OramaZettaCluster` can carry model checkpoints that every node finds on its own data volume
at `/data/models/<name>` when it starts. The first one is the **Orama base model**, which every
Lake workspace Zetta carries so a native agent created without a model can chat
([lake#246](https://github.com/oramasearch/lake/issues/246),
[lake#799](https://github.com/oramasearch/lake/issues/799)).

## How a model gets there

```yaml
spec:
  pullSecretName: dockerhub
  models:
    - name: base-agent
      image: oramasearch/base-agent-0.6b:v16
```

Each entry is an **init container** `model-<name>` on the node pod. It runs the entry's image,
which is a **model image**: an OCI image holding the checkpoint files under `/model` (for a
Qwen-style checkpoint: `config.json`, `tokenizer.json`, `tokenizer_config.json`, `vocab.json`,
`merges.txt`, `model.safetensors`, …) and a POSIX shell at `/bin/sh`. The container mounts the
node's data volume at `/data` and runs a constant script (`internal/controller/zetta_models.go`;
the directory and the image arrive as arguments, never as script text, per operator#279):

1. If `/data/models/<name>/.zetta-model-image` already names this image, it does nothing. A node
   restart copies nothing.
2. Otherwise it copies `/model` into `/data/models/<name>.staging`, writes the marker, removes the
   old directory and renames the staging directory into place. A half-finished copy is never taken
   for a model, and a new image replaces the whole previous copy.
3. If any step fails (a full volume, an image without `/model`), it removes the staging directory,
   logs `the node starts without it`, and exits 0. **A model never holds a node's data offline**;
   a native agent's chat then reports that the checkpoint is missing.

The init container runs as the node's user (uid 10001), drops every capability, and requests
`10m` CPU and `32Mi` memory (limit `1` CPU, `256Mi`), so a workspace namespace's quota and
LimitRange admit it. It is pulled with `pullSecretName` and `IfNotPresent`: **use a tag that is
never moved, or a digest**, because the copy on the volume changes only when the image string
changes.

### Why an image, and not a download

A Lake workspace namespace denies egress by default (`docs/lake/workspaces.md`): its pods reach
lake, DNS and each other, and the internet only with a per-workspace `internetEgress` override.
A pod that downloaded from Hugging Face would need that route and a Hugging Face token in every
workspace namespace. An image is pulled by the kubelet, outside the pod's network policy, with the
pull Secret the workspace already has, and each node caches it, so a second workspace on the same
node pulls nothing.

## The Orama base model in every workspace

The Lake workspace reconciler sets every workspace Zetta's `spec.models` to

```yaml
- name: base-agent
  image: <spec.workspaces.baseModelImage, else the default in internal/controller/lake_defaults.go>
```

and the lake container gets `LAKE_BASE_AGENT_MODEL=/data/models/base-agent` (a reserved variable:
`spec.env` cannot move it). lake fills that path in as the `model` of a native agent created
without one (lake `docs/api/train-agents.md`), and the workspace Zetta loads the checkpoint from
it when the agent chats.

The default is `oramasearch/base-agent-0.6b:v16`: the checkpoint published privately as
[`OramaSearch/base-agent-0.6b`](https://huggingface.co/OramaSearch/base-agent-0.6b) folder `v16`,
built into an image by zetta's `images/base-agent/`. Moving the version is a change to
`lake_defaults.go`, rolled dev, testing, staging, prod like any default image.

**Room for it.** The fp32 checkpoint is about 2.2 GiB on the volume (lake's default workspace
volume is 5 GiB) and about 2.4 GiB in memory once loaded, so the default workspace Zetta memory
limit is `4Gi` (`spec.workspaces.resources` overrides it). The request stays `64Mi`: a workspace
that never chats with a native agent uses no more than before.

**CPU, not GPU, today.** The Zetta image is built with `ZETTA_ENABLE_CUDA=OFF`, so the base model
runs on CPU on whatever node the workspace Zetta is on. Running it on a GPU node needs a CUDA
Zetta image and GPU nodes labelled `orama.cloud/gpu-class` on the cluster; that is tracked
separately.

## Checking it

```sh
# The init container's log says what it did.
kubectl -n <lake>-ws-<id> logs <cluster>-zetta-0 -c model-base-agent
# The checkpoint and the image it came from.
kubectl -n <lake>-ws-<id> exec <cluster>-zetta-0 -c zetta -- ls /data/models/base-agent
kubectl -n <lake>-ws-<id> exec <cluster>-zetta-0 -c zetta -- cat /data/models/base-agent/.zetta-model-image
```

Both are reads (`exec` running `ls` or `cat` writes nothing).
