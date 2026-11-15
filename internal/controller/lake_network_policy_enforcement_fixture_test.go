package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The package's fake clients and envtest API server run no Cilium, so every test in the package
// sees a cluster that enforces network policy unless it supplies its own check
// (LakeReconciler.NetworkPolicyCheck) or calls ciliumNetworkPolicyEnforcement directly, as
// lake_network_policy_enforcement_test.go does. Production never replaces the check.
func init() {
	defaultNetworkPolicyEnforcementCheck = func(context.Context, client.Reader, int64) networkPolicyEnforcement {
		return enforcedForTest
	}
}

var enforcedForTest = networkPolicyEnforcement{Enforced: true, Condition: networkPolicyConditionForTest(true, reasonNetworkPolicyEnforced)}
