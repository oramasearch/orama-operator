# Managed sets

- [lake-nonprod.yaml](lake-nonprod.yaml): dev, testing and staging with separate Stripe sandboxes.
- [lake-prod.yaml](lake-prod.yaml): production EU and US. Both members declare operator-managed
  live payments from managing-cluster Secret `lake/stripe-live`, copied as `stripe`; each has its
  own operator-managed webhook and signing Secret. EU passed its gate before the US change.
  The common legacy `lake-prod-stripe` copy is removed from this set; retained Secret objects
  are not deleted. It also carries, commented out, the restore rehearsals of both members
  (`spec.restoreRehearsals`: the store now, the control database once it is backed up), until the
  managing cluster runs an operator that knows the field
  ([runbook](../../../docs/lake/restore-rehearsals.md), #127).
- [v1-nonprod.yaml](v1-nonprod.yaml): Cloud v1 dev, with no Prod 1 migrations or migration-only
  Secret references. Its managing-cluster pilot source Secrets await Steve (operator#979).
- [v1-prod.yaml](v1-prod.yaml): Cloud v1 EU and US, preserving the production cutover settings.
  Cleanup removes 37 successful migration declarations, keeping customer data because the live
  CRs have `removeOnDelete` false/unset. The failed `team-aee7dc1a` catch-up and its four Secret
  references remain. Archive required run evidence before applying; Juan approves the production
  apply under D8. Steve approves any nonprod apply and separate source-Secret retirement.
  [Cleanup behavior and checks](../README.md#copying-a-prod-1-team-into-a-member).

Apply only after the credential checks and production approval in
[operator#643](https://github.com/oramasearch/orama-operator/issues/643). Creating these references
in Git does not establish that live payments are active. See the
[cluster runbook](../README.md) and [Lake payment procedure](../../../docs/lake/payments.md).
