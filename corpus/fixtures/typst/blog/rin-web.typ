#let rinWebPage(id) = context {
  if target() == "html" {
    html.elem("span", attrs: (id: "rin-page-" + id))
  }
}
