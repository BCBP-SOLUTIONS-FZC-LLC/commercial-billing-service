# Request to Platform Edge — OP-G2: Gateway JWT Provider for the Platform Realm

| Field | Value |
|---|---|
| From | Commercial Team (Billing), with IAM |
| To | Platform edge (owner of the ingress Envoy Gateway) |
| Date | 2026-11-19 |
| Item | CD-52 operator path, step **OP-G2** (AuthZ Enrichment LLD §8.9.1; IAM HLD v1.59 §5.4, §17.2). Billing BL-OQ-14 |
| Why now | OP-G2 is the only external blocker for every `/api/v1/operator/*` route, IAM's and Commercial's, including Billing's Phase 1 conversion stub. Until it exists, all operator calls fail through the gateway |
| Deployment state | All services dev-only; start in dev |

## Where the three steps stand

| Step | What | Owner | State |
|---|---|---|---|
| OP-G1 | Keycloak platform realm `bcbp-platform` (`platform_operator` role, realm-wide OTP, 5-min tokens, 8-h max session) | IAM (Realm Provisioner) | **Done:** [iam-realm-provisioner#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60) merged (`9e8fcab`). Created automatically at Realm Provisioner startup on its next deploy |
| **OP-G2** | **Gateway `jwt_authn` provider for the platform realm, on `/api/v1/operator/*` only** | **Platform edge** | **This request** |
| OP-G3 | `OPERATOR_REALM_ISSUER` (and `JWT_OPERATOR_PAYLOAD_KEY`) in AuthZ Enrichment, per environment | IAM (AuthZ Enrichment) | Last: needs OP-G1 deployed and the provider name from OP-G2 |
| OP-G4 | End-to-end test through the gateway | IAM + Commercial | After OP-G3. Per-route expectations already pinned in AuthZ tests ([iam-authz-enrichment#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43), merged) |

OP-G1 and OP-G2 can proceed in parallel: the issuer URL is fixed by the realm name.

## What we need

A JWT provider on the ingress Envoy Gateway that:

1. **Trusts the platform realm:**
   - issuer: `https://auth.bcbpsolutions.com/realms/bcbp-platform`
   - JWKS: `https://auth.bcbpsolutions.com/realms/bcbp-platform/protocol/openid-connect/certs`
   - Keycloak host per Realm Provisioner LLD §13.1. **Confirm the dev Keycloak's public host.** If dev uses a different host, the issuer must match the `iss` claim dev tokens actually carry.
2. **Applies only on `/api/v1/operator/*`.** Platform-realm tokens must not authenticate tenant routes, and tenant-realm tokens must not authenticate operator routes. AuthZ Enrichment also enforces both rules (defence in depth: `DENY_ROLE_INSUFFICIENT`), but the gateway is the first control.
3. **Keeps `ext_authz` (AuthZ Enrichment) on operator routes.** AuthZ is what injects `x-tenant-roles: platform_operator`, `x-user-id` and `x-tenant-id`; without it the services refuse every operator call.
4. **Writes the verified payload into `jwt_authn` filter metadata**, as the tenant providers do. The verified payload must carry `sub`, `iss` and `realm_access.roles`. Tell IAM the **metadata key** it uses: in Envoy Gateway that is the provider name. AuthZ reads it from `JWT_OPERATOR_PAYLOAD_KEY` (empty means the same key as tenant tokens, today `trial`).
5. **Strips client-supplied identity headers** (`x-user-id`, `x-tenant-id`, `x-tenant-roles`, …) as on tenant routes.

## Suggested shape (Envoy Gateway; your call on the final form)

Services expose operator paths on their own operator `HTTPRoute`s (Billing: a separate operator route, Billing LLD §13.1). One option is a `SecurityPolicy` per operator route carrying only the platform provider, merged with the Gateway-level policy so `ext_authz` still applies:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: operator-platform-jwt            # one per operator HTTPRoute, or one listing all of them
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: <service>-operator           # e.g. Billing's operator HTTPRoute
  mergeType: StrategicMerge              # keep the Gateway-level ext_authz (Envoy Gateway v1.8+)
  jwt:
    providers:
      - name: platform                   # → JWT_OPERATOR_PAYLOAD_KEY=platform in AuthZ (OP-G3)
        issuer: https://auth.bcbpsolutions.com/realms/bcbp-platform
        remoteJWKS:
          uri: https://auth.bcbpsolutions.com/realms/bcbp-platform/protocol/openid-connect/certs
```

The Gateway-level tenant providers must not also accept tenant tokens on these routes. If a merged policy would keep them, attach the operator routes to a listener or policy without them instead.

## Done when

- In dev, a `bcbp-platform` token (user holding `platform_operator`, OTP completed) reaches a `/api/v1/operator/...` route and AuthZ logs `operator_access`.
- A tenant-realm token on `/api/v1/operator/...` gets `403`; a platform token on a tenant route gets `403`.
- Platform edge has told IAM the provider/metadata key name.

## Then (OP-G3, IAM)

`iam-authz-enrichment` per-environment values:

```yaml
OPERATOR_REALM_ISSUER: "https://auth.bcbpsolutions.com/realms/bcbp-platform"
JWT_OPERATOR_PAYLOAD_KEY: "platform"     # the provider name from OP-G2; empty if it shares the tenant key
```

Then redeploy AuthZ Enrichment. Startup refuses an operator issuer equal to a tenant realm's issuer. After that the joint OP-G4 run (A-3) can start in dev.

## Asks

1. A target date for OP-G2 in dev. This also answers IAM's OP-G6 (date to Commercial, open since 28 Oct).
2. The dev Keycloak public host, if not `auth.bcbpsolutions.com`.
3. The metadata key / provider name you choose.
