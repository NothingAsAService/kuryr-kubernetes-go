#!/usr/bin/env bash
#
# Restores the two service accounts that helmify collapses into one.
#
# helmify models a chart as having a single service account: every
# ServiceAccount it reads becomes the same `<chart>.serviceAccountName`
# template, and every workload and every RoleBinding is pointed at it. Kuryr
# has two identities on purpose. The controller writes to the Kubernetes API
# and to Neutron; the node agent only reads, and runs privileged on every node.
# Accepting helmify's output would hand that privileged DaemonSet the
# controller's full ClusterRole, which is a worse deal than it looks.
#
# So this puts the split back. Every edit below asserts what it expects to find
# first: a helmify upgrade that changes the generated shape has to fail here,
# loudly, rather than quietly produce a chart that over-permissions the node
# agent.
set -euo pipefail

chart="${1:?usage: $0 CHART_DIR}"
name="$(basename "${chart}")"

helpers="${chart}/templates/_helpers.tpl"
accounts="${chart}/templates/serviceaccount.yaml"
daemonset="${chart}/templates/daemonset.yaml"
cnirbac="${chart}/templates/cni-rbac.yaml"

for f in "${helpers}" "${accounts}" "${daemonset}" "${cnirbac}"; do
    [ -f "${f}" ] || { echo "$0: ${f} does not exist; did helmify run?" >&2; exit 1; }
done

collapsed="include \"${name}.serviceAccountName\""

# expect FILE COUNT -- fail unless the collapsed helper appears exactly COUNT
# times, which is what makes every substitution below safe to apply blindly.
expect() {
    local file="$1" want="$2" got
    got=$(grep -c -F "${collapsed}" "${file}" || true)
    if [ "${got}" != "${want}" ]; then
        echo "$0: expected ${want} reference(s) to ${collapsed} in ${file}, found ${got}." >&2
        echo "$0: helmify's output has changed shape; re-check this script before trusting the chart." >&2
        exit 1
    fi
}

expect "${accounts}" 2
expect "${daemonset}" 1
expect "${cnirbac}" 1

# The node agent's account, named after the controller's so the two stay
# visibly related and a release still owns everything it creates.
cat >> "${helpers}" <<TPL

{{/*
Service account of the CNI node agent.

It is deliberately not the controller's: the agent runs privileged on every
node and only ever reads, so it is bound to a read-only ClusterRole of its own.
*/}}
{{- define "${name}.cniServiceAccountName" -}}
{{- printf "%s-cni" (include "${name}.serviceAccountName" .) }}
{{- end }}
TPL

# Both service account blocks helmify wrote are identical, so rather than
# patching one of two indistinguishable copies, replace the file outright.
cat > "${accounts}" <<SA
{{ if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "${name}.serviceAccountName" . }}
  labels:
  {{- include "${name}.labels" . | nindent 4 }}
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
automountServiceAccountToken: {{ .Values.serviceAccount.automount }}
{{- end }}
---
{{ if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "${name}.cniServiceAccountName" . }}
  labels:
  {{- include "${name}.labels" . | nindent 4 }}
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
automountServiceAccountToken: {{ .Values.serviceAccount.automount }}
{{- end }}
SA

# The DaemonSet and its ClusterRoleBinding are the two places that have to name
# the agent's account instead of the controller's.
sed -i "s/${name}\.serviceAccountName/${name}.cniServiceAccountName/" "${daemonset}"
sed -i "s/${name}\.serviceAccountName/${name}.cniServiceAccountName/" "${cnirbac}"

echo "$0: split the CNI node agent's service account out of ${chart}"
