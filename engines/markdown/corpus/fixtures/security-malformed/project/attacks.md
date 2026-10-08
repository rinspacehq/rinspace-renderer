# 安全边界

<script>alert("never execute")</script>

<iframe src="https://attacker.invalid"></iframe>

[危险链接](javascript:alert(1))

![危险图片](data:image/svg+xml,<svg onload=alert(1)>)

<rin-work data-id="rw_00000000000000000000000000000000"></rin-work>

:::widget{execute="true"}
不受支持的指令必须保持惰性。
:::

```unknown-runtime
</code><script>alert(1)</script>
```

<div><em>故意不闭合的原始 HTML
