# The `script-bundle` image of the Pixie release (#455, docs/infrastructure/observability.md): PxL's
# script bundle, served inside the hub so neither the UI nor the script manager fetches it from
# artifacts.px.dev or Google Cloud Storage. Built from bundle-oss.json made at the same
# oramasearch/pixie commit as every other image (see hack/pixie-patches/README.md, "Building").
#
# Serves /pixie-scripts/bundle.json on :8080 as a non-root user: the proxy maps /pxl_scripts/ to it,
# and the script manager reads it as http://$STORAGE_EMULATOR_HOST/pixie-scripts/bundle.json.
# Pass the base image by digest from Orama's mirror when building; nginx is BSD-2-Clause.
ARG NGINX_IMAGE
FROM ${NGINX_IMAGE}
COPY default.conf /etc/nginx/conf.d/default.conf
COPY bundle-oss.json /usr/share/nginx/html/pixie-scripts/bundle.json
EXPOSE 8080
