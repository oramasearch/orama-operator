# Managed sets

- [lake-nonprod.yaml](lake-nonprod.yaml): dev, testing and staging with separate Stripe sandboxes.
- [lake-prod.yaml](lake-prod.yaml): production EU and US. Both members declare operator-managed
  live payments from managing-cluster Secret `lake/stripe-live`, copied as `stripe`; each has its
  own operator-managed webhook and signing Secret. EU passed its gate before the US change.
  The common legacy `lake-prod-stripe` copy is removed from this set; retained Secret objects
  are not deleted.
- [v1-nonprod.yaml](v1-nonprod.yaml) and [v1-prod.yaml](v1-prod.yaml): Cloud v1 instances.

Apply only after the credential checks and production approval in
[operator#643](https://github.com/oramasearch/orama-operator/issues/643). Creating these references
in Git does not establish that live payments are active. See the
[cluster runbook](../README.md) and [Lake payment procedure](../../../docs/lake/payments.md).
