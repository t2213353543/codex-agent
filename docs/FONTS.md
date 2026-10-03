# 管理后台字体

管理界面使用自托管的 **Codex Admin Sans**，由 Noto Sans SC 可变字体制作界面字符子集。正文使用 400，导航与按钮使用 500，标题、表头和数字使用 600；同一 WOFF2 文件覆盖三个字重，避免浏览器合成粗体。API token、ID 等需要等宽显示的字段仍使用系统等宽字体。

## 来源与交付文件

- 原文件：本机 `C:/Windows/Fonts/NotoSansSC-VF.ttf`，17,773,244 bytes。
- 真实字体 metadata：`Noto Sans SC`，`Version 2.04;241114210130;non-release`，原始 `wght` 轴 100–900，30,890 个 Unicode codepoints。此处记录实际文件版本，不将其称为新的官方发布版本。
- 原文件 SHA-256：`763146584cf0710223441356b4395e279021b0806c196614377a7a0174ae074a`。
- 字体许可：SIL Open Font License 1.1；嵌入版权是 Adobe 2014–2021，保留名称为 Source。许可正文取自 [Google Fonts 官方 Noto Sans SC 目录](https://github.com/google/fonts/blob/main/ofl/notosanssc/OFL.txt)，与源字体版权一致；[Noto CJK 官方许可](https://github.com/notofonts/noto-cjk/blob/main/Sans/LICENSE)可交叉核对。子集字体使用独立名称 Codex Admin Sans，保留原版权与许可 metadata。
- 产物：`frontend/public/assets/fonts/codex-admin-sans-0058e27ebaf7d129.woff2`，**136,812 bytes（133.6 KiB）**，SHA-256 `0058e27ebaf7d129c1eb9880ab09622e3eb75911ba26e0e595762ea453185eff`。
- 许可正文和完整覆盖清单：同目录 `OFL.txt`、`font-metadata.json`。

全部字体请求均发往本站 `/assets/fonts/`，不使用外部字体 CDN。该路径适配现有后端静态资源路由；文件名包含内容哈希，支持现有不可变资源和旧页面兼容机制。`font-display: swap` 避免阻塞文字显示。

当前子集有 **693 个 codepoints**，包含全部 `frontend/src` 固定界面文字、Latin-1 范围内原字体支持的字符以及常用合成数据名称。生成后已再次核对最新 Dashboard 与所有界面源码，固定文字没有缺字。动态账号名中不在子集的字符，由 Noto Sans SC、PingFang SC、Segoe UI、Microsoft YaHei 和通用 sans-serif 按系统支持情况回退；界面主字体不依赖 Microsoft YaHei。未提供完整三万字字体，避免仅为任意动态名称让首屏下载完整 17 MB 源文件。

## 精确复现

依赖为 `fonttools==4.65.0`、`brotli==1.2.0`。在项目根目录执行以下 PowerShell 命令，读取现有 metadata 中的字符清单，在内存中重建字体并验证哈希；验证成功后写回相同文件。此命令已验证产物 SHA-256 完全一致。

```powershell
@'
from pathlib import Path
from fontTools.ttLib import TTFont
from fontTools import subset
from fontTools.varLib.instancer import instantiateVariableFont
import hashlib, io, json

directory = Path('frontend/public/assets/fonts')
metadata = json.loads((directory / 'font-metadata.json').read_text(encoding='utf-8'))
source = Path('C:/Windows/Fonts/NotoSansSC-VF.ttf')
assert hashlib.sha256(source.read_bytes()).hexdigest() == metadata['source_sha256']
font = TTFont(source, recalcTimestamp=False)
options = subset.Options()
options.hinting = False
options.name_IDs = ['*']
options.name_legacy = True
options.name_languages = ['*']
subsetter = subset.Subsetter(options=options)
subsetter.populate(unicodes=[int(cp[2:], 16) for cp in metadata['codepoints']])
subsetter.subset(font)
instantiateVariableFont(font, {'wght': (400, 400, 600)}, inplace=True, optimize=True)
for record in font['name'].names:
    if record.nameID in [1, 4, 6, 16]:
        value = 'CodexAdminSans' if record.nameID == 6 else 'Codex Admin Sans'
    elif record.nameID in [2, 17]:
        value = 'Regular'
    else:
        continue
    record.string = value.encode(record.getEncoding(), errors='replace')
font['head'].modified = 3872489169
font.flavor = 'woff2'
buffer = io.BytesIO()
font.save(buffer)
data = buffer.getvalue()
assert hashlib.sha256(data).hexdigest() == metadata['sha256']
(directory / metadata['filename']).write_bytes(data)
print('PASS: byte-identical font reproduction', len(data))
'@ | python -
```

需要增加字符时，从最新源码收集字符并和现有清单合并，更新 metadata、字体文件名与 `style.css` 引用，再执行页面验收；不要在内容变化后继续使用旧哈希文件名。

## 浏览器验证

只检查 `getComputedStyle(...).fontFamily` 无法证明字体成功绘制。验收应等待 `document.fonts.ready`，检查 WOFF2 请求为 200；再通过 Chromium CDP `CSS.getPlatformFontsForNode` 检查 `.page-header-title`、`.logo-name`、导航文字节点的实际 `familyName` 为 `Codex Admin Sans` 且 `isCustomFont` 为 `true`，并检查 `glyphCount` 大于零。同时保留桌面/手机截图，检查中文基线、表头换行和控件尺寸。

本次共享样式增加蓝、青、靛、琥珀辅助色、图标底座、轻浅阴影和标题层级；56px 顶栏、36px 页签、透明加载层与 32px 中性圆环没有改变，减少动画时圆环仍保留完整固定弧线。
