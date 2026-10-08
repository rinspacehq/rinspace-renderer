#set text(font: "WenQuanYi Zen Hei", lang: "zh")
#set heading(numbering: "1.")
#import "rin-web.typ": rinWebPage

#rinWebPage("intro")
= 引言 <intro>

这是多文件书籍。第一章定义见 @definition，第四章回引也应工作。

#include "chapters/one.typ"
#include "chapters/two.typ"
#include "chapters/three.typ"
#include "chapters/four.typ"

#rinWebPage("appendix")
= 附录 <appendix>

附录回引 @group-theory。

#bibliography("refs.bib")
