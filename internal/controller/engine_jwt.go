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
// JWT_ISSUER, audience the project's writer URL (the control DB's oramacore_clusters.writer_url),
// keys from the dashboard's /api/.well-known/jwks.json. OramaCore's JWKS provider matches issuer and
// audience exactly, so the dashboard and the writer read the issuer from one place, jwtIssuer(oc).

// dashboardJWKSPath is where the dashboard serves its public signing key (JWT_PUBLIC_KEY) as a JWKS.
const dashboardJWKSPath = "/api/.well-known/jwks.json"

// jwtFieldMaxLength is the CRD's MaxLength on spec.jwtIssuer and each audience, checked again here
// because a fake client or a direct controller call does not run admission.
const jwtFieldMaxLength = 128

// writerJWTAudiences is Audiences without duplicates, in first-occurrence order. The strings are
// kept as written: a trailing slash, scheme or case change is a different audience to OramaCore.
func writerJWTAudiences(cfg *oramav1alpha1.WriterJWTSpec) []string {
	out := make([]string, 0, len(cfg.Audiences))
	for _, a := range cfg.Audiences {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// writerJWKSURL is the configured JWKS URL, or the in-cluster dashboard Service's when unset, so the
// writer fetches keys from a pod in its own namespace instead of through public DNS and Cloudflare.
// Callers check engine.writer.jwt is set first.
func writerJWKSURL(oc *oramav1alpha1.OramaCloudV1) string {
	if u := oc.Spec.Components.Engine.Writer.JWT.JWKSURL; u != "" {
		return u
	}
	return fmt.Sprintf("http://%s-dashboard:3000%s", oc.Name, dashboardJWKSPath)
}

// writerJWKSFromDashboard reports whether the writer renders a JWT provider whose JWKS host is this
// stack's dashboard Service (<cr>-dashboard, optionally qualified with the namespace, .svc and the
// cluster domain). The network sandbox opens writer to dashboard TCP 3000 only then; an external
// JWKS goes out through allow-engine-public-internet instead.
func writerJWKSFromDashboard(oc *oramav1alpha1.OramaCloudV1) bool {
	if oc.Spec.Components.Engine.Writer.JWT == nil {
		return false
	}
	u, err := url.Parse(writerJWKSURL(oc))
	if err != nil {
		return false
	}
	service := oc.Name + "-dashboard"
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	switch host {
	case service, service + "." + oc.Namespace, service + "." + oc.Namespace + ".svc":
		return true
	}
	return strings.HasPrefix(host, service+"."+oc.Namespace+".svc.")
}

// validateWriterJWT refuses a writer JWT configuration that would reject every write: no issuer, no
// audience or an empty one, a field over its length limit, a public writer host whose exact URL
// (https://<host>) is not an audience, or no usable JWKS URL. It checks the configured strings
// only; it neither reads the control database nor fetches the JWKS.
func validateWriterJWT(oc *oramav1alpha1.OramaCloudV1) error {
	cfg := oc.Spec.Components.Engine.Writer.JWT
	if cfg == nil {
		return nil
	}
	const field = "spec.components.engine.writer.jwt"
	switch {
	case oc.Spec.JWTIssuer == "":
		return fmt.Errorf("%s requires spec.jwtIssuer: set it to the issuer the dashboard signs write tokens with", field)
	case len(oc.Spec.JWTIssuer) > jwtFieldMaxLength:
		return fmt.Errorf("spec.jwtIssuer is longer than %d characters", jwtFieldMaxLength)
	case len(cfg.Audiences) == 0:
		return fmt.Errorf("%s.audiences needs at least one entry: each writer URL the dashboard signs tokens for", field)
	}
	for i, a := range cfg.Audiences {
		if a == "" || len(a) > jwtFieldMaxLength {
			return fmt.Errorf("%s.audiences[%d] must be 1 to %d characters", field, i, jwtFieldMaxLength)
		}
	}
	// The dashboard signs a write token for the project's writer URL, and OramaCore compares the
	// audience exactly: a public writer host missing from the audiences would refuse every write.
	if host := oc.Spec.Components.Engine.Writer.Host; host != "" {
		if want := "https://" + host; !slices.Contains(writerJWTAudiences(cfg), want) {
			return fmt.Errorf("%s.audiences must include %q, the URL of spec.components.engine.writer.host, exactly as written (no trailing slash)", field, want)
		}
	}
	if cfg.JWKSURL == "" {
		if !oc.Spec.Components.Dashboard.Enabled {
			return fmt.Errorf("%s.jwksURL is required when the dashboard is disabled", field)
		}
		return nil
	}
	u, err := url.Parse(cfg.JWKSURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("%s.jwksURL must be an http or https URL with a host and no credentials", field)
	}
	return nil
}

// writerJWTYAML is the writer_side.jwt block, indented to sit under writer_side, or "" when
// engine.writer.jwt is unset. Every user string is a quoted, escaped YAML scalar.
func writerJWTYAML(oc *oramav1alpha1.OramaCloudV1) string {
	cfg := oc.Spec.Components.Engine.Writer.JWT
	if cfg == nil {
		return ""
	}
	auds := writerJWTAudiences(cfg)
	quoted := make([]string, len(auds))
	for i, a := range auds {
		quoted[i] = strconv.Quote(a)
	}
	return fmt.Sprintf(`  jwt:
    providers:
      - name: dashboard
        jwks_url: %s
        issuers: [%s]
        audiences: [%s]
        refresh_interval: 1h
`, strconv.Quote(writerJWKSURL(oc)), strconv.Quote(jwtIssuer(oc)), strings.Join(quoted, ", "))
}
