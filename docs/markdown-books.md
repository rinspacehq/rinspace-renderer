# Markdown book entrypoint

Both Markdown articles and books use `main.md`. An article contains its body.
A book contains a leading title and a nested list of relative chapter paths:

```markdown
# Book title

- introduction.md
- chapter1.md
- chapter2.md
  - sections/section2-1.md
```

The title is optional metadata, not a chapter. When present, it takes precedence
over the submitted project title. Only one nonempty leading level-one heading
is accepted; prose, later headings and additional configuration formats are not.

List order defines reading order. Two spaces add one level, up to six levels and
512 chapters. Paths must name existing Markdown files inside the project, must
not be duplicated or refer to `main.md`, and must not traverse hidden directories.
The catalog contributes to project identity but is never rendered as a chapter.
Chapter identities depend on their paths and remain stable when reordered.
