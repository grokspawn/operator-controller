#! /bin/bash

set -o errexit
set -o nounset
set -o pipefail

help="setup.sh is used to build extensions using the operator-sdk and
build the image + bundle image, and create a FBC image for the
following bundle formats:
- registry+v1
This script will ensure that all images built are loaded onto
a KinD cluster with the name specified in the arguments.
The following environment variables are required for configuring this script:
- \$CATALOG_IMG - the tag for the catalog image that contains the registry+v1 bundle.
- \$REG_PKG_NAME - the name of the package for the extension that uses the registry+v1 bundle format.
- \$LOCAL_REGISTRY_HOST - hostname:port of the local docker-registry
setup.sh also takes 5 arguments.

Usage:
  setup.sh [OPERATOR_SDK] [CONTAINER_RUNTIME] [KUSTOMIZE] [KIND] [KIND_CLUSTER_NAME] [NAMESPACE]
"

########################################
# Input validation
########################################

if [[ "$#" -ne 6 ]]; then
  echo "Illegal number of arguments passed"
  echo "${help}"
  exit 1
fi

if [[ -z "${CATALOG_IMG}" ]]; then
  echo "\$CATALOG_IMG is required to be set"
  echo "${help}"
  exit 1
fi

if [[ -z "${REG_PKG_NAME}" ]]; then
  echo "\$REG_PKG_NAME is required to be set"
  echo "${help}"
  exit 1
fi

if [[ -z "${LOCAL_REGISTRY_HOST}" ]]; then
  echo "\$LOCAL_REGISTRY_HOST is required to be set"
  echo "${help}"
  exit 1
fi

########################################
# Setup temp dir and local variables
########################################

# We're going to do file manipulation, so let's work in a temp dir
TMP_ROOT="$(mktemp -d ./tmp.XXXXXX)"
# Make sure to delete the temp dir when we exit
trap 'chmod -R +w ${TMP_ROOT} && rm -rf ${TMP_ROOT}' EXIT

DOMAIN=oc-opdev-e2e.operatorframework.io
REG_DIR="${TMP_ROOT}/registry"
mkdir -p "${REG_DIR}"

operator_sdk=$1
container_tool=$2
kustomize=$3
kind=$4
kcluster_name=$5
namespace=$6

reg_img="${DOMAIN}/registry:v0.0.1"
reg_bundle_img="${LOCAL_REGISTRY_HOST}/bundles/registry-v1/registry-bundle:v0.0.1"

catalog_img="${CATALOG_IMG}"
reg_pkg_name="${REG_PKG_NAME}"

########################################
# Create the registry+v1 based extension
# and build + load images
########################################

# controller-gen v0.13.0 (scaffolded by operator-sdk) panics when run with
# go 1.22, so pin to a more recent version.
# NOTE: This is a rough edge that users will experience

# The Makefile in the project scaffolded by operator-sdk uses an SDK binary
# in the path path if it is present. Override via `export` to ensure we use
# the same version that we scaffolded with.
# NOTE: this is a rough edge that users will experience

(
  cd "${REG_DIR}" && \
  $operator_sdk init --domain="${DOMAIN}" && \
  sed -i -e 's/CONTROLLER_TOOLS_VERSION ?= v0.13.0/CONTROLLER_TOOLS_VERSION ?= v0.15.0/' Makefile && \
  $operator_sdk create api \
    --group="${DOMAIN}" \
    --version v1alpha1 \
    --kind Registry \
    --resource --controller && \
  export OPERATOR_SDK="${operator_sdk}" && \
  make generate manifests && \
  make docker-build IMG="${reg_img}" && \
  sed -i -e 's/$(OPERATOR_SDK) generate kustomize manifests -q/$(OPERATOR_SDK) generate kustomize manifests -q --interactive=false/g' Makefile && \
  make bundle IMG="${reg_img}" VERSION=0.0.1 && \
  make bundle-build BUNDLE_IMG="${reg_bundle_img}"
)

###############################
# Create the FBC that contains
# the registry+v1 extensions
###############################

cat << EOF > "${TMP_ROOT}"/catalog.Dockerfile
FROM scratch
ADD catalog /configs
LABEL operators.operatorframework.io.index.configs.v1=/configs
EOF

mkdir -p "${TMP_ROOT}/catalog"
cat <<EOF > "${TMP_ROOT}"/catalog/index.yaml
{
  "schema": "olm.package",
  "name": "${reg_pkg_name}"
}
{
  "schema": "olm.bundle",
  "name": "${reg_pkg_name}.v0.0.1",
  "package": "${reg_pkg_name}",
  "image": "${reg_bundle_img}",
  "properties": [
    {
      "type": "olm.package",
      "value": {
        "packageName": "${reg_pkg_name}",
        "version": "0.0.1"
      }
    }
  ]
}
{
  "schema": "olm.channel",
  "name": "preview",
  "package": "${reg_pkg_name}",
  "entries": [
    {
      "name": "${reg_pkg_name}.v0.0.1"
    }
  ]
}
EOF

# Add a .indexignore to make catalogd ignore
# reading the symlinked ..* files that are created when
# mounting a ConfigMap
cat <<EOF > "${TMP_ROOT}"/catalog/.indexignore
..*
EOF

kubectl create configmap -n "${namespace}" --from-file="${TMP_ROOT}"/catalog.Dockerfile extension-dev-e2e.dockerfile
kubectl create configmap -n "${namespace}" --from-file="${TMP_ROOT}"/catalog extension-dev-e2e.build-contents

kubectl apply -f - << EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: kaniko
  namespace: "${namespace}"
spec:
  template:
    spec:
      containers:
      - name: kaniko
        image: gcr.io/kaniko-project/executor:latest
        args: ["--dockerfile=/workspace/catalog.Dockerfile",
                "--context=/workspace/",
                "--destination=${catalog_img}",
                "--skip-tls-verify"]
        volumeMounts:
          - name: dockerfile
            mountPath: /workspace/
          - name: build-contents
            mountPath: /workspace/catalog/
      restartPolicy: Never
      volumes:
        - name: dockerfile
          configMap:
            name: extension-dev-e2e.dockerfile
            items:
              - key: catalog.Dockerfile
                path: catalog.Dockerfile
        - name: build-contents
          configMap:
            name: extension-dev-e2e.build-contents
EOF

kubectl wait --for=condition=Complete -n "${namespace}" jobs/kaniko --timeout=60s

# Make sure all files are removable. This is necessary because
# the Makefiles generated by the Operator-SDK have targets
# that install binaries under the bin/ directory. Those binaries
# don't have write permissions so they can't be removed unless
# we ensure they have the write permissions
chmod -R +w "${REG_DIR}/bin"

# Load the bundle image into the docker-registry

kubectl create configmap -n "${namespace}" --from-file="${REG_DIR}/bundle.Dockerfile" operator-controller-e2e-${reg_pkg_name}.root

tgz="${REG_DIR}/manifests.tgz"
tar czf "${tgz}" -C "${REG_DIR}" bundle
kubectl create configmap -n "${namespace}" --from-file="${tgz}" operator-controller-${reg_pkg_name}.manifests

kubectl apply -f - << EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: "kaniko-${reg_pkg_name}"
  namespace: "${namespace}"
spec:
  template:
    spec:
      initContainers:
        - name: copy-manifests
          image: busybox
          command: ['sh', '-c', 'cp /manifests-data/* /manifests']
          volumeMounts:
            - name: manifests
              mountPath: /manifests
            - name: manifests-data
              mountPath: /manifests-data
      containers:
      - name: kaniko
        image: gcr.io/kaniko-project/executor:latest
        args: ["--dockerfile=/workspace/bundle.Dockerfile",
                "--context=tar:///workspace/manifests/manifests.tgz",
                "--destination=${reg_bundle_img}",
                "--skip-tls-verify"]
        volumeMounts:
          - name: dockerfile
            mountPath: /workspace/
          - name: manifests
            mountPath: /workspace/manifests/
      restartPolicy: Never
      volumes:
        - name: dockerfile
          configMap:
            name: operator-controller-e2e-${reg_pkg_name}.root
        - name: manifests
          emptyDir: {}
        - name: manifests-data
          configMap:
            name: operator-controller-${reg_pkg_name}.manifests
EOF

kubectl wait --for=condition=Complete -n "${namespace}" jobs/kaniko-${reg_pkg_name} --timeout=60s
