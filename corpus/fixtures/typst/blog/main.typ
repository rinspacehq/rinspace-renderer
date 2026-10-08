#set text(font: "WenQuanYi Zen Hei", lang: "zh")
#set heading(numbering: "1.")
#import "rin-web.typ": rinWebPage

#rinWebPage("intro")
= 中文与数学 <intro>

这是一篇 Typst 文章，包含*强调*、`inline code`、[外部链接](https://typst.app)和公式。

$ a^2 + b^2 = c^2 $

#rinWebPage("assets")
== 表格与图片 <assets>

#table(
  columns: 2,
  [项目], [结果],
  [中文], [$alpha + beta$],
)

#image("diagram.svg", width: 45%)

```typst
#let answer = 42
```

#rinWebPage("references")
== 引用

参见 @typst-paper，以及 @intro。

#bibliography("refs.bib")
