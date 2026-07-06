# owrt-lite
A lightweight proxy tool
# Owrt-Lite 用户手册

## v6 修复重点

- 修复 SIP002 Shadowsocks 节点解析：支持 `ss://base64(method:password)@host:port`。
- 兼容已经保存过的错误 SS 节点：运行时会自动解码 Base64 用户信息，避免 `ss cipher err: cipher not supported`。
- 节点列表中 SS 的加密方式和密码显示也会自动还原为正确值。
- 登录页重新居中并调整了页面颜色和卡片样式。
- 保留 v5 的 Trojan 支持、Windows 系统代理修复、节点运行状态、代理池/代理链轮转等功能。

VMess 仍不建议作为实际出口。

## 使用建议

1. 运行 v6；
2. 登录后台；
3. 如果之前保存了旧节点，仍可直接使用，v6 会自动兼容；
4. 如仍异常，建议清空节点后重新导入订阅/节点；
5. 选择节点后点击“检测当前 IP”。

## Windows 系统代理

Windows 设置里应显示：

```text
代理 IP 地址：127.0.0.1
端口：8080 或实际端口
```

## 编译

```bash
go build -trimpath -ldflags "-s -w" -o owrt-lite.exe ./cmd/owrt-lite
```
