# 截图存放目录

把仪表板截图放到本目录,文件名遵循下表(README 引用按这个名字):

| 文件名 | 截图内容 |
|---|---|
| `dashboard-overview.png` | 主页全景:左栏设备列表 + 右栏打卡事件流 |
| `worktime-tab.png` | 工作时长 tab:月度汇总卡 + 每日列表 |
| `monthly-stats.png` | 月统计 tab:近 12 月聚合表 |
| `notify-tab.png` | 信息设置 tab:Webhook + ntfy 表单 + res 主题消息 |
| `device-detail.png`(可选) | 单台设备点开后的详情面板 |

## 截图建议

* 浏览器宽度 1280px 左右,系统主题深色(项目默认深色配色)
* macOS 用 `Cmd+Shift+5` → 选区域 → 保存为 PNG
* Chrome / Edge 可装"Awesome Screenshot"插件做整页截图
* 截图前给设备设个友好别名 + 配几条手动工时,看起来更真实
* 不要把真实 MAC 完整暴露在截图里,可以马赛克掉后三段或用 dummy 数据
* 单图建议 < 500KB,大于就用 [tinypng.com](https://tinypng.com) 压一下

## 提交

```bash
git add docs/screenshots/*.png
git commit -m "docs: add UI screenshots"
git push
```

README 自动生效;不需要别的步骤。
