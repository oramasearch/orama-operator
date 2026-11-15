/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// operator#782: the operator holds lake's maxima itself.
func TestMethodologySizeRefusalHoldsLakesMaxima(t *testing.T) {
	if refusal := methodologySizeRefusal(16000, 64<<30, 8, 24*3600); refusal != "" {
		t.Fatalf("lake's maxima refused: %s", refusal)
	}
	for name, refusal := range map[string]string{
		"cpu":     methodologySizeRefusal(16001, 1, 0, 1),
		"memory":  methodologySizeRefusal(1, 64<<30+1, 0, 1),
		"gpus":    methodologySizeRefusal(1, 1, 9, 1),
		"timeout": methodologySizeRefusal(1, 1, 0, 24*3600+1),
	} {
		if refusal == "" {
			t.Errorf("%s over lake's maximum was accepted", name)
		}
	}
}

// operator#782: a sandbox or serve image lake names must be in the Lake's own registry.
func TestMethodologyImageRefusalRequiresTheLakesRegistry(t *testing.T) {
	lake := &oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{Host: "lake.example.com",
		CustomDomain: &oramav1alpha1.LakeCustomDomainSpec{Host: "data.example.org"}}}
	digest := "@sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	for image, refused := range map[string]bool{
		"lake.example.com/space/model" + digest:             false,
		"data.example.org/space/model" + digest:             false,
		"docker.io/attacker/model" + digest:                 true,
		"lake.example.com.attacker.io/space/model" + digest: true,
	} {
		if (methodologyImageRefusal(lake, image) != "") != refused {
			t.Errorf("%s: refused=%v", image, !refused)
		}
	}
	if methodologyImageRefusal(&oramav1alpha1.Lake{}, "lake.example.com/x"+digest) == "" {
		t.Error("a Lake without a host accepted an image")
	}
}
