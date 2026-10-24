# Managed sets

- [lake-nonprod.yaml](lake-nonprod.yaml): dev, testing and staging with separate Stripe sandboxes.
- [lake-prod.yaml](lake-prod.yaml): production EU and US. The EU member declares operator-managed
  live payments from managing-cluster Secret `lake/stripe-live`, copied as `stripe`. The US member
  retains the legacy manual-payment configuration until the EU gate passes and its own change lands.
- [v1-nonprod.yaml](v1-nonprod.yaml) and [v1-prod.yaml](v1-prod.yaml): Cloud v1 instances.

Apply only after the credential checks and production approval in
[operator#643](https://github.com/oramasearch/orama-operator/issues/643). Creating these references
in Git does not establish that live payments are active. See the
[cluster runbook](../README.md) and [Lake payment procedure](../../../docs/lake/payments.md).
