// SPDX-License-Identifier: MIT

package controller

import "testing"

func TestWithOwnedAnnotationKeepsOtherControllersKeys(t *testing.T) {
	got := withOwnedAnnotation(map[string]string{"deployment.kubernetes.io/revision": "3", "owner": "old"}, "owner", "new")
	if got["deployment.kubernetes.io/revision"] != "3" || got["owner"] != "new" || len(got) != 2 {
		t.Fatalf("annotations = %#v", got)
	}
	if got := withOwnedAnnotation(nil, "owner", "new"); got["owner"] != "new" || len(got) != 1 {
		t.Fatalf("nil annotations = %#v", got)
	}
}
