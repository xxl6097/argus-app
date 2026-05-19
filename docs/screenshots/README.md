# 截图存放目录

把仪表板截图放到本目录, 文件名遵循下表 (主 README 按这个名字引用):

| 文件名 | 截图内容 |
|---|---|
| `dashboard-overview.png` | 主页全景: 左栏设备列表 + 右栏打卡事件流 + 顶部 ⚙ 设置按钮 |
| `worktime-tab.png` | 工作时长 tab: 月度汇总卡 + 每日列表 |
| `monthly-stats.png` | 月统计 tab: 近 12 月聚合表 |
| `notify-tab.png` | 信息设置 tab: Webhook + ntfy 表单 + res 主题消息 |
| `device-detail.png` | 单台设备点开后的详情面板 (含「设为打卡」 / 「踢下线」 徽章) |
| `settings.png` | ⚙ 系统设置 modal: 全局通知 webhook + 钉钉关键词 + 账户 + 系统 + 备份与恢复 |
| `upgrade.png` | 版本徽章 + 在线检测升级 modal |

## 截图建议

- 浏览器宽度 1280px 左右, 系统主题深色 (项目默认深色配色)
- macOS 用 `Cmd+Shift+5` → 选区域 → 保存为 PNG
- Chrome / Edge 可装 "Awesome Screenshot" 插件做整页截图
- 截图前给设备设个友好别名 + 配几条手动工时, 看起来更真实
- **不要把真实 MAC / IP / token / 路由器 hostname 暴露在截图里**:
  - 用本目录的 `mask_macs.py` 把 MAC 地址自动打码: `python3 mask_macs.py screenshot.png`
  - webhook 中带 access_token 的地址手动遮罩
  - 路由器 hostname (clife.b5 等) 用图片编辑器打个 mosaic
- 单图建议 < 500KB, 大于就用 [tinypng.com](https://tinypng.com) 压一下

## 提交

```bash
git add docs/screenshots/*.png
git commit -m "docs: refresh UI screenshots"
git push
```

主 README 引用的是相对路径, 自动生效, 不需要别的步骤。

> 发布流程 (`v*.*.*` tag) 不会动 `docs/`, 所以截图变更只在 git 历史里, 不进
> Release 资产包。
