# 房间投票 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 完成限时多数投票、房主否决和管理权限移交。
**Architecture:** 服务端统一裁决状态，客户端同步和重试，Fyne 提供投票条与否决窗。管理房主与游戏来源分离。
**Tech Stack:** Go, UDP, Fyne 2.8.1, Windows/Linux server.
**Spec:** `docs/superpowers/specs/2026-10-16-room-voting-design.md`

## Global Constraints

- 投票 60 秒、否决 30 秒；目标不参与，发起时名单固定，严格过半。
- 原房主保留成员身份；成功后发起人接任。每房间一个进行中投票。
- 原媒体消息不变。既有 Fyne 120 Hz 构建补丁保留。
- 当前文件夹无 Git；直接工作，备份到 `.build/voting-before/`，不创建仓库。

## Review Focus

- 乱序状态不得恢复旧房主；重试命令不得重复执行。
- 关闭否决窗不否决，旧连接对话框不得作用到新连接。
- 服务器截止时间与身份校验不可由客户端绕过。
- 管理权限移交不能改变仍在线的游戏服务来源。
- 被踢的读循环清理不能等待自身。

## Tasks

- [x] 协议与服务端：`internal/proto/room_vote.go`, `internal/relay/room_vote.go`, 小范围修改 `server.go`；先失败测试，再实现严格多数/否决/截止/去重/权限/来源隔离与成员退出测试。
- [x] 隧道客户端：`internal/tunnel/room_vote.go`, 小范围修改 `client.go`；先失败测试，再接入同步、重试、角色更新、拒绝清理与管理/游戏来源分离。
- [x] Fyne 界面：`cmd/relayclient/room_vote.go`, 小范围修改 `main.go`, `flat_ui.go`；先失败测试，再提供成员菜单、全局投票条、一次性否决弹窗和连接隔离。
- [x] 联调：真实本机中继、多成员投票、否决与无响应执行、权限和原房主保留；全量回归、代码审查并修复。
- [x] 构建 Windows 客户端、Windows/Linux 服务端；检查运行中的文件，更新未被占用的交付文件，补充使用说明和检查记录。

## Decisions

- 用户补充确认：原房主不踢出，成功后移交给发起人；排除目标；60/30 秒。
- 名单固定，发起人/目标离开取消；普通名单成员退出不重新计算，避免途中改变门槛。
- 控制同步同时修复原有自动接任未更新客户端权限的问题。

## Verification

- 全量回归通过；真实 UDP 多客户端的五项流程检查通过。
- 原生弹窗明确否决、关闭后完整等待 30 秒、原房主留房与游戏来源保留通过；原有屏幕播放与切档检查通过。
- 审查发现状态消息先于加入回复会导致连接失败，已先复现再修复，并复审通过。
- 不改动审查中确认的既有显式断开/游戏套接字边界问题及既有直接踢人确认窗行为；未由本功能引入。
- 交付文件与校验值见 `tests/room_vote_results_2026-10-16.md`。
