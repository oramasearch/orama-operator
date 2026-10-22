#!/usr/bin/env python3
"""Render admission restrictions from the pinned Karpenter bundle; requires PyYAML."""
import json
from pathlib import Path
import yaml

root = Path(__file__).resolve().parent.parent
bundle = list(yaml.safe_load_all((root / 'internal/controller/assets/karpenter/v1.13.0/controller.yaml').read_text()))
rbac = [o for o in bundle if o and o['kind'] in ('Role', 'ClusterRole', 'RoleBinding', 'ClusterRoleBinding')]
j = lambda value: json.dumps(value, separators=(',', ':'))

def policy(name, resources, validations, scope='*', manager=True):
    p = {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy',
         'metadata': {'name': 'orama-operator-' + name}, 'spec': {
             'failurePolicy': 'Fail', 'matchConstraints': {'resourceRules': [{
                 'apiGroups': ['*'], 'apiVersions': ['*'], 'operations': ['CREATE', 'UPDATE', 'DELETE'],
                 'resources': resources, 'scope': scope}]},
             'matchConditions': [{'name': 'manager', 'expression':
                 "request.userInfo.username == 'system:serviceaccount:{{ .Release.Namespace }}:{{ .Values.serviceAccount.name }}'"}],
             'validations': [{'expression': e, 'message': m, 'reason': 'Forbidden'} for e, m in validations]}}
    b = {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicyBinding',
         'metadata': {'name': 'orama-operator-' + name},
         'spec': {'policyName': 'orama-operator-' + name, 'validationActions': ['Deny']}}
    return [p, b]

objects = []
label = "'app.kubernetes.io/managed-by'"
managed = lambda obj: f"has({obj}.metadata.labels) && {label} in {obj}.metadata.labels && {obj}.metadata.labels[{label}] == 'orama-operator'"
objects += policy('namespaces', ['namespaces', 'namespaces/*'], [
    ("!request.name.startsWith('kube-') && request.name != 'default'", 'The manager cannot change reserved namespaces'),
    (f"request.operation == 'CREATE' ? ({managed('object')}) : ({managed('oldObject')})", 'Existing namespaces require a trusted managed-by label; the manager cannot adopt them'),
    (f"request.operation == 'DELETE' || ({managed('object')})", 'Managed namespace ownership cannot be removed by the manager'),
], 'Cluster')
# This also blocks indirect writes through third-party workload CRDs in protected namespaces.
objects += policy('namespace-writes', ['*/*'], [
    ("request.namespace == '' || (!request.namespace.startsWith('kube-') && request.namespace != 'default') || "
     "(request.namespace == 'kube-system' && request.name in ['karpenter', 'karpenter-dns'] && "
     "((request.resource.group == 'apps' && request.resource.resource == 'deployments' && request.name == 'karpenter') || "
     "(request.resource.group == '' && request.resource.resource in ['serviceaccounts', 'services'] && request.name == 'karpenter') || "
     "(request.resource.group == 'policy' && request.resource.resource == 'poddisruptionbudgets' && request.name == 'karpenter') || "
     "(request.resource.group == 'rbac.authorization.k8s.io' && request.resource.resource in ['roles', 'rolebindings'])))",
     'The manager can change only the pinned Karpenter objects in kube-system'),
    (f"request.namespace == '' || (request.resource.group == 'orama.cloud' && request.subResource == 'status') || request.namespace in ['kube-system', '{{{{ .Release.Namespace }}}}'] || (namespaceObject != null && ({managed('namespaceObject')}))",
     'Workload namespaces require a trusted managed-by label'),
])
# Pin every RBAC rule field, including the fields omitted by the bundle. Object names alone
# do not prevent replacing a permitted role with a wildcard role or rebinding it to the manager.
allowed_roles = []
allowed_bindings = []
for o in rbac:
    base = f"request.kind.kind == '{o['kind']}' && request.name == '{o['metadata']['name']}' && request.namespace == '{o['metadata'].get('namespace', '')}'"
    if 'rules' in o:
        checks = [base, f"object.rules.size() == {len(o['rules'])}", f"object.metadata.labels == {j(dict(o['metadata']['labels'], **{'orama.cloud/karpenter-install': 'true', 'app.kubernetes.io/managed-by': 'orama-cloud-v1-operator'}))}"]
        if o['kind'] == 'ClusterRole': checks.append('!has(object.aggregationRule)')
        for i, rule in enumerate(o['rules']):
            for field in ['apiGroups', 'resources', 'verbs', 'resourceNames', 'nonResourceURLs']:
                at = f'object.rules[{i}].{field}'
                checks.append(f'{at} == {j(rule[field])}' if field in rule else f'(!has({at}) || {at}.size() == 0)')
        allowed_roles.append('(' + ' && '.join(checks) + ')')
    else:
        checks = [base]
        for field, value in o['roleRef'].items(): checks.append(f'object.roleRef.{field} == {j(value)}')
        checks.append('object.subjects.size() == 1')
        for field, value in o['subjects'][0].items(): checks.append(f'object.subjects[0].{field} == {j(value)}')
        checks.append("(!has(object.subjects[0].apiGroup) || object.subjects[0].apiGroup == '')")
        allowed_bindings.append('(' + ' && '.join(checks) + ')')
objects += policy('roles', ['roles', 'clusterroles'], [
    ("request.operation == 'DELETE' || " + ' || '.join(allowed_roles), 'Manager role writes must exactly match the vendored Karpenter rules')])
workload_binding = "(request.kind.kind == 'RoleBinding' && request.name == 'orama-operator-workloads' && request.namespace != 'kube-system' && " \
    "object.roleRef.apiGroup == 'rbac.authorization.k8s.io' && object.roleRef.kind == 'ClusterRole' && object.roleRef.name == 'orama-operator-workloads' && " \
    "object.subjects.size() == 1 && object.subjects[0].kind == 'ServiceAccount' && object.subjects[0].name == '{{ .Values.serviceAccount.name }}' && " \
    "object.subjects[0].namespace == '{{ .Release.Namespace }}' && (!has(object.subjects[0].apiGroup) || object.subjects[0].apiGroup == ''))"
objects += policy('bindings', ['rolebindings', 'clusterrolebindings'], [
    ("request.operation == 'DELETE' || " + workload_binding + ' || ' + ' || '.join(allowed_bindings), 'Manager bindings must target its namespaced workload role or the pinned Karpenter account')])
container = 'object.spec.template.spec.containers[0]'
pod = 'object.spec.template.spec'
karpenter = next(o for o in bundle if o and o['kind'] == 'Deployment')
env = karpenter['spec']['template']['spec']['containers'][0]['env']
checks = [f'{pod}.serviceAccountName == "karpenter"', f'{pod}.containers.size() == 1',
          f'{container}.name == "controller"', f'{container}.image == {j(karpenter["spec"]["template"]["spec"]["containers"][0]["image"])}']
for field in ['command', 'args', 'envFrom', 'volumeMounts', 'volumeDevices']:
    checks.append(f'(!has({container}.{field}) || {container}.{field}.size() == 0)')
for field in ['initContainers', 'ephemeralContainers', 'volumes', 'imagePullSecrets']:
    checks.append(f'(!has({pod}.{field}) || {pod}.{field}.size() == 0)')
for field in ['hostNetwork', 'hostPID', 'hostIPC', 'shareProcessNamespace']:
    checks.append(f'(!has({pod}.{field}) || !{pod}.{field})')
checks += [f'object.spec.template.metadata.labels == {j(karpenter["spec"]["template"]["metadata"]["labels"])}', f'(!has(object.spec.template.metadata.annotations) || object.spec.template.metadata.annotations.size() == 0)', f'{container}.securityContext.runAsNonRoot', f'{container}.securityContext.runAsUser == 65532', f'{container}.securityContext.readOnlyRootFilesystem', f'!{container}.securityContext.privileged', f'!{container}.securityContext.allowPrivilegeEscalation', f'{container}.securityContext.capabilities.drop == ["ALL"]', f'(!has({container}.securityContext.capabilities.add) || {container}.securityContext.capabilities.add.size() == 0)', f'!has({container}.lifecycle)', f'{container}.env.size() == {len(env)}']
for i, e in enumerate(env):
    at = f'{container}.env[{i}]'
    checks.append(f'{at}.name == {j(e["name"])}')
    if e['name'] == 'CLUSTER_NAME':
        checks += [f'!has({at}.valueFrom)', f"{at}.value.matches('^[0-9A-Za-z][A-Za-z0-9_-]{{0,99}}$')"]
    elif 'value' in e:
        checks += [f'!has({at}.valueFrom)', f'{at}.value == {j(e["value"])}']
    else:
        for source, fields in e['valueFrom'].items():
            for field, value in fields.items():
                checks.append(f'{at}.valueFrom.{source}.{field} == {j(str(value))}')
for probe in ['livenessProbe', 'readinessProbe', 'startupProbe']:
    checks.append(f'(!has({container}.{probe}) || (!has({container}.{probe}.exec) && !has({container}.{probe}.tcpSocket) && !has({container}.{probe}.grpc)))')
objects += policy('karpenter-pod', ['deployments'], [
    ("request.namespace != 'kube-system' || request.name != 'karpenter' || request.operation == 'DELETE' || (" + ' && '.join(checks) + ')',
     'Karpenter must retain its pinned image, executable, environment and isolated pod identity')], 'Namespaced')
# AdmissionRequest omits empty optional strings for cluster-scoped and core-group requests.
# Normalize them before evaluating the rules; a missing key must never break bootstrap.
for obj in objects:
    if obj['kind'] != 'ValidatingAdmissionPolicy':
        continue
    obj['spec']['variables'] = [
        {'name': 'targetNamespace', 'expression': "has(request.namespace) ? request.namespace : ''"},
        {'name': 'apiGroup', 'expression': "has(request.resource.group) ? request.resource.group : ''"},
        {'name': 'subresource', 'expression': "has(request.subResource) ? request.subResource : ''"},
    ]
    for validation in obj['spec']['validations']:
        for before, after in [('request.namespace', 'variables.targetNamespace'), ('request.resource.group', 'variables.apiGroup'), ('request.subResource', 'variables.subresource')]:
            validation['expression'] = validation['expression'].replace(before, after)
        if obj['metadata']['name'] == 'orama-operator-namespace-writes':
            validation['expression'] = "(variables.apiGroup == '' && request.resource.resource == 'namespaces') || (" + validation['expression'] + ")"
# Namespace objects have their own policy; Kubernetes also supplies their name as request.namespace.
# The namespace exemption above covers this special API convention without weakening that policy.
# YAML quoting preserves CEL strings and Helm template substitutions.
out = yaml.safe_dump_all(objects, sort_keys=False, width=120)
(root / 'chart/templates/manager-admission.yaml').write_text('# Generated by hack/generate-manager-admission.py from the pinned Karpenter bundle.\n' + out)
