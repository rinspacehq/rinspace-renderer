# LaTeXML Engine

This directory owns the LaTeXML adapter and version lock.

LaTeXML is a replaceable document conversion engine behind Rin Renderer. Clients must depend on Rin Renderer protocol responses, not raw LaTeXML HTML/XML.

Current API integration uses `latexmlc` through `rin-renderer/api/internal/latexmladapter`.

Rin Renderer does not delegate project diagrams blindly to LaTeXML. The project API runs a diagram prepass first, replacing supported Rin diagram blocks and commands with stable text placeholders. Those diagrams are rendered by the Rin TeX SVG pipeline and persisted to CloudBase, then the placeholders in LaTeXML HTML are replaced with CloudBase-backed `<figure class="rin-tikz ...">` markup.

The adapter contract is intentionally CLI-shaped first:

```text
latexmlc --expire=-1 --format=html5 --destination=<out.html> --log=<latexml.log> --includestyles <main.tex>
```

`--includestyles` is required because a Rinspace work declares its own macros in
its own sources: without the flag LaTeXML loads only its bundled bindings and
reports every project-declared macro as undefined.

Future work may move this behind a long-running worker or direct Perl API without changing Rin Renderer public endpoints.

Runtime images must carry `latexml.lock` and pass `deploy/verify-latexml-runtime.sh`. The verifier checks the pinned LaTeXML commit, Perl version, TeXLive version, adapter version presence, and a minimal conversion smoke test before an image is considered usable.
