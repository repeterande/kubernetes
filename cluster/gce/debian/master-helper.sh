#!/bin/bash

# Copyright 2015 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# A library of helper functions and constant for debian os distro


# create-master-instance creates the master instance. If called with
# an argument, the argument is used as the name to a reserved IP
# address for the master. (In the case of upgrade/repair, we re-use
# the same IP.)
#
# It requires a whole slew of assumed variables, partially due to to
# the call to write-master-env. Listing them would be rather
# futile. Instead, we list the required calls to ensure any additional
# variables are set:
#   ensure-temp-dir
#   detect-project
#   get-bearer-token
#
function create-master-instance {
  local address_opt=""
  [[ -n ${1:-} ]] && address_opt="--address ${1}"
  local preemptible_master=""
  if [[ "${PREEMPTIBLE_MASTER:-}" == "true" ]]; then
    preemptible_master="--preemptible --maintenance-policy TERMINATE"
  fi

  write-master-env
  prepare-startup-script
  create-master-instance-internal "${MASTER_NAME}" "${address_opt}" "${preemptible_master}"
}

function replicate-master-instance() {
  local existing_master_zone="${1}"
  local existing_master_name="${2}"
  local existing_master_replicas="${3}"

  local kube_env="$(get-metadata "${existing_master_zone}" "${existing_master_name}" kube-env)"
  # Substitute INITIAL_ETCD_CLUSTER to enable etcd clustering.
  kube_env="$(echo "${kube_env}" | grep -v "INITIAL_ETCD_CLUSTER")"
  kube_env="$(echo -e "${kube_env}\nINITIAL_ETCD_CLUSTER: '${existing_master_replicas},${REPLICA_NAME}'")"
  echo "${kube_env}" > ${KUBE_TEMP}/master-kube-env.yaml
  get-metadata "${existing_master_zone}" "${existing_master_name}" cluster-name > ${KUBE_TEMP}/cluster-name.txt
  get-metadata "${existing_master_zone}" "${existing_master_name}" startup-script > ${KUBE_TEMP}/configure-vm.sh

  create-master-instance-internal "${REPLICA_NAME}"
}

function create-master-instance-internal() {
  local -r master_name="${1}"
  local -r address_option="${2:-}"
  local -r preemptible_master="${3:-}"

  gcloud compute instances create "${master_name}" \
    ${address_option} \
    --project "${PROJECT}" \
    --zone "${ZONE}" \
    --machine-type "${MASTER_SIZE}" \
    --image-project="${MASTER_IMAGE_PROJECT}" \
    --image "${MASTER_IMAGE}" \
    --tags "${MASTER_TAG}" \
    --network "${NETWORK}" \
    --scopes "storage-ro,compute-rw,monitoring,logging-write" \
    --can-ip-forward \
    --metadata-from-file \
      "startup-script=${KUBE_TEMP}/configure-vm.sh,kube-env=${KUBE_TEMP}/master-kube-env.yaml,cluster-name=${KUBE_TEMP}/cluster-name.txt" \
    --disk "name=${master_name}-pd,device-name=master-pd,mode=rw,boot=no,auto-delete=no" \
    --boot-disk-size "${MASTER_ROOT_DISK_SIZE:-10}" \
    ${preemptible_master}
}

# TODO: This is most likely not the best way to read metadata from the existing master.
function get-metadata() {
  local zone="${1}"
  local name="${2}"
  local key="${3}"
  gcloud compute ssh "${name}" \
    --project "${PROJECT}" \
    --zone="${zone}" \
    --command "curl \"http://metadata.google.internal/computeMetadata/v1/instance/attributes/${key}\" -H \"Metadata-Flavor: Google\"" 2>/dev/null
}
