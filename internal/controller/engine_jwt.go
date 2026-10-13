/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The writer verifies the RS256 write tokens the dashboard's POST /api/user/jwt signs: issuer
// JWT_ISSUER, audience the project's clusterWriterUrl (the control DB's oramacore_clusters.writer_url),
// keys from the dashboard's /api/.well-known/jwks.json. OramaCore's dashboard JWKS provider checks
// the issuer and audience by exact match, so both sides must read them from the same place:
// jwtIssuer(oc) for the issuer, and spec.components.engine.writer.jwt for the audiences.

// dashboardJWKSPath is where the dashboard serves its public signing key as a JWKS.
const dashboardJWKSPath = "/api/.well-known/jwks.json"

// writerJWTAudiences is the union of the deprecated single audience and the audience list, in
// order, without duplicates.
func writerJWTAudiences(cfg *oramav1alpha1.WriterJWTSpec) []string {
	var out []string
	for _, a := range append([]string{cfg.Audience}, cfg.Audiences...) {
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// writerJWKSURL is the configured JWKS URL, or the in-cluster dashboard Service's when unset, so
// the writer fetches keys from a Pod in its own namespace instead of through public DNS and the edge.
func writerJWKSURL(oc *oramav1alpha1.OramaCloudV1) string {
	if u := oc.Spec.Components.Engine.Writer.JWT.JWKSURL; u != "" {
		return u
	}
	return fmt.Sprintf("http://%s-dashboard:3000%s", oc.Name, dashboardJWKSPath)
}

// validateWriterJWT refuses a writer JWT config that would reject every customer write: no
// explicit issuer, no audience, an unusable JWKS URL, or an audience list that does not contain the
// writer's own public URL.
func validateWriterJWT(oc *oramav1alpha1.OramaCloudV1) error {
	w := oc.Spec.Components.Engine.Writer
	cfg := w.JWT
	if cfg == nil {
		return nil
	}
	if oc.Spec.JWTIssuer == "" {
		return fmt.Errorf("engine.writer.jwt requires spec.jwtIssuer: set it to the JWT_ISSUER the dashboard signs write tokens with")
	}
	audiences := writerJWTAudiences(cfg)
	if len(audiences) == 0 {
		return fmt.Errorf("engine.writer.jwt requires at least one audience (the clusterWriterUrl the dashboard signs tokens for)")
	}
	if cfg.JWKSURL == "" {
		if !oc.Spec.Components.Dashboard.Enabled {
			return fmt.Errorf("engine.writer.jwt.jwksURL is required when the dashboard is disabled")
		}
	} else if u, err := url.Parse(cfg.JWKSURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("engine.writer.jwt.jwksURL must be an HTTP(S) URL without credentials")
	}
	if w.Host != "" {
		if public := "https://" + w.Host; !slices.Contains(audiences, public) {
			return fmt.Errorf("engine.writer.jwt audiences %v do not include %s, the writer's public URL (engine.writer.host); the dashboard signs each token for the exact clusterWriterUrl it returns", audiences, public)
		}
	}
	return nil
}

// writerJWTYAML is the writer_side jwt block, or "" when writer JWT verification is off.
func writerJWTYAML(oc *oramav1alpha1.OramaCloudV1) string {
	cfg := oc.Spec.Components.Engine.Writer.JWT
	if cfg == nil {
		return ""
	}
	quoted := make([]string, 0, len(cfg.Audiences)+1)
	for _, a := range writerJWTAudiences(cfg) {
		quoted = append(quoted, strconv.Quote(a))
	}
	return fmt.Sprintf("  jwt:\n    providers:\n      - name: dashboard\n        jwks_url: %q\n        issuers: [%q]\n        audiences: [%s]\n        refresh_interval: 1h\n",
		writerJWKSURL(oc), jwtIssuer(oc), strings.Join(quoted, ", "))
}
