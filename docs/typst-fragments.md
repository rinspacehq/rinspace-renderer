# Private Typst fragments

`POST /api/render/fragments` submits `typst-fragments/v1` with an exact
`renderProfileId` and an ordered `blocks` array of `{id, source}`. It uses the
existing authenticated job API, `Idempotency-Key` and owner-scope header. It
does not create a public article, book or publication identity. The normal
job status/result endpoints recover the accepted job.

The submission response's `X-Rin-Fragment-Project-SHA256` is the canonical
project graph hash used in the final result and document bundle. The separate
`X-Rin-Fragment-Source-SHA256` is the SHA-256 of the exact private ZIP artifact.
They commit different representations and must not be interchanged. Queue
admission continues to verify the exact uploaded artifact bytes.

All blocks compile as one trusted wrapper, preserving native Typst equation
numbering and forward references. IDs are unique lowercase identifiers. The
runtime requires the pinned compiler/font/package set and reviewed sandbox
binary/hash; its fragments profile includes the wrapper and isolation identity.
When the queue worker starts as root so it can use the shared artifact store,
it owns only the preparation phase: the bubblewrap child and its private
workspace are transferred to UID/GID 65534 before namespace creation. The
container therefore needs only `CHOWN`, `SETUID` and `SETGID`; bubblewrap drops
all capabilities inside the new namespace.
An ordinary synchronous or unisolated runtime cannot admit these requests.

The Typst worker uses the existing HTML/MathML, MathJax CHTML and final-output
pipeline. A fragment result is a single semantic v1 document bundle with stable
`fragment-<id>` roots. There is no PDF/image fallback, package download or public
asset publication. Generated resource-bearing content fails explicitly. Limits
are 128 blocks, 4 MiB of source and 8 MiB of final HTML. Consumers verify the
profile, canonical project identity, bundle hash and ordered block roots before
displaying content.
