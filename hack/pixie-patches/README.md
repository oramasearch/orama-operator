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
