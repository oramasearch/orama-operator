/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

type uploadsEnvNames struct{ Endpoint, Bucket, Region, AccessKeyID, SecretAccessKey string }

var (
	dashboardUploadsEnv      = uploadsEnvNames{"S3_ENDPOINT", "S3_BUCKET", "S3_BUCKET_REGION", "AWS_ACCESS_KEY_VALUE", "AWS_SECRET_ACCESS_KEY_VALUE"}
	crawlerUploadsEnv        = uploadsEnvNames{"AWS_ENDPOINT_URL_S3", "S3_BUCKET_NAME", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	importConsumerUploadsEnv = uploadsEnvNames{"S3_ENDPOINT", "S3_BUCKET", "AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
)

// uploadsBucketEnv maps the uploads bucket's credentials Secret to one S3 client's variable names.
// All three clients share one token scoped to this staging bucket; no account credential leaves it.
// References are required; dashboardUploadsBucketEnv makes only the dashboard's references optional.
func uploadsBucketEnv(oc *oramav1alpha1.OramaCloudV1, names uploadsEnvNames) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, entry := range []struct{ name, key string }{
		{names.Endpoint, "endpoint"}, {names.Bucket, "bucket"}, {names.Region, "region"},
		{names.AccessKeyID, "accessKeyId"}, {names.SecretAccessKey, "secretAccessKey"},
	} {
		env = append(env, corev1.EnvVar{Name: entry.name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{
				Name: oc.Name + "-uploads-r2"}, Key: entry.key},
		}})
	}
	return env
}

// dashboardUploadsBucketEnv lets the dashboard keep its envFrom values while the bucket Secret is
// absent. The credential hash rolls it onto R2 once the Secret appears.
func dashboardUploadsBucketEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	env := uploadsBucketEnv(oc, dashboardUploadsEnv)
	for i := range env {
		env[i].ValueFrom.SecretKeyRef.Optional = boolPtr(true)
	}
	return env
}
