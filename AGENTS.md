# Renderer repository rules

This repository is the standalone source for Rinspace Renderer after the
maintainer accepts its public-source cutover. Until that acceptance, the
authoritative implementation remains in the private `rinspace/rin-renderer/`
tree. A public commit or tag alone does not change the product's source or
deployed artifact.

Keep local rendering usable without a Rinspace account or CloudBase. Keep the
Control Plane profile compatible with the versioned protocol snapshots and
private product integration. Never add product credentials, real user works,
private deployment overlays, or production logs to this repository.

Changes to an artifact require an immutable release with exact source commit,
OCI digest or file SHA-256, engine locks, capability declaration, SBOM, and
notices. Rinspace consumes the same reviewed bytes by exact digest after
public checks and a separate private integration decision. Do not point the
product at `main`, `latest`, a mutable tag, or a pull-request build. External
contributions do not synchronize into the private product automatically.

Source owned by Rinspace is AGPL-3.0-only; third-party licenses remain their
own. Review dependency and image licenses again when their locks change.
