# Native admission fixtures

`internal/controller/testdata/k0s/` contains Apache-2.0 k0s fixtures from
[commit cc922054ad10fd1991ab1077bb8a2c3bf3eeeae2](https://github.com/oramasearch/k0s/commit/cc922054ad10fd1991ab1077bb8a2c3bf3eeeae2)
on PR #41 (`issue-40-tenant-profile-rbac`). That PR defines the tenant grant; the two admission
policies are unchanged from the issue #155 baseline commit `c930955e8923606ee222886a0b209cb105eb97f5`.

- `environment-policies.yaml` contains the four complete admissionregistration/v1 documents extracted
  verbatim from `static/manifests/liquid/platform/platform.yaml`, retaining their license header.
- `tenant-profile.yaml` is the complete `static/manifests/liquid/tenant-namespace/profile.yaml` template.
  The test substitutes the namespace argument and installs its Role and RoleBinding. Namespace,
  network, quota and limits come from the real operator reconciliation path.

To refresh, retrieve those two files at a reviewed k0s commit, preserve the policy documents exactly,
update `k0sDeliveryContract` in the test, and run the native proof in
[environments.md](environments.md#native-admission-regression). Do not change a fixture to bypass an
admission denial. The proof requires both policies and both bindings, asserts the namespace selector,
and checks actual denied and admitted API requests. It separately renders and installs the operator
chart's current RBAC; those permissions are not duplicated in a fixture.
