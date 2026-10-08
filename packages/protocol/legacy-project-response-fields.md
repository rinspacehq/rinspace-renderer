# Legacy Project Response Field Classification

`ProjectRenderResponse` remains the synchronous compatibility shape. New job/result APIs must use
`rin-render-result/v1`; they must not copy private debug data into the canonical result.

| Class | Existing fields | Policy |
| --- | --- | --- |
| Canonical projection | `requestId`, `engine`, `assets`, `generatedArtifacts`, `versions`, `diagnostics` | Derived deterministically from the canonical result. `generatedArtifacts` is the compatibility proof for immutable math/diagram objects and becomes canonical `assets`. |
| Compatibility-only | `title`, `html`, `fallback`, `primaryEngine`, `fallbackEngine`, `mainFile`, `diagrams`, `assetFiles`, `assetManifest`, `reader`, `math` | Emitted only by the existing synchronous compatibility route while clients migrate. |
| Private debug | `texSource`, `source`, `analysisSource`, `resolvedSource`, `assetInventory`, `project` | Stored only in private, expiring job artifacts and omitted unless an authorized internal debug path explicitly requests them. |

The compatibility mapper has an explicit `includePrivateDebug` input. Its default is false, so a
canonical result cannot accidentally expose source merely because debug metadata exists.
