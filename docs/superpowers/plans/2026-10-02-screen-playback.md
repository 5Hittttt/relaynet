# Screen Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 播放端独立选择分辨率和帧率，并降低低档位接收带宽。
**Architecture:** 带编号的观看订阅经中继发送到共享者，按需共用捕获及编码。视频订阅编号隔离切档，音频共享原来的时间线。
**Tech Stack:** Go、Fyne、WGC、Media Foundation、现有 UDP 加密中继。
**Spec:** ../specs/2026-10-02-screen-playback-design.md

## Global Constraints

- Windows 客户端，Windows/Linux 中继；100 Mbps 单端视频限速。
- 不超过共享端配置；视频 P 帧不能在编码后为降帧任意丢弃。
- 修改现有链路，保留 RAV1 和旧客户端兼容。

## Review Focus

- 切换成员与切档控制包乱序：旧请求不得恢复旧档位。
- 断开或停止共享：编码线程与捕获资源全部回收。
- 同档位新加入：等待关键帧后恢复，不能重置其他观看者。
- 订阅控制丢包：定期重发后恢复；未观看仍能发现共享者。
- 低帧率和高档位互切：拒绝旧视频，音频时钟保持连续。

### Task 1: Protocol and relay

Files: internal/proto/screen.go, internal/proto/screen_subscription.go, internal/relay/server.go, corresponding tests.
Interfaces: ScreenSubscription{RequestID, Active, Resolution, FPS}; Encode/DecodeScreenSubscription; ScreenPayload.SubscriptionID.
- [x] Write failing RAV2 and relay broadcast isolation tests, observe RED.
- [x] Validate subscription fields and request ordering; encode RAV2 only for subscribed video.
- [x] Filter broadcast video for negotiated viewers, validate direct source, test GREEN.

### Task 2: On-demand capture outputs and subscription sessions

Files: internal/media/wgc.go, video.go, internal/tunnel/screen.go, screen_share.go, client.go, tests.
Interfaces: VideoCapturer.AddOutput(q, callback) func(); Client.SetScreenPlayback(resolution byte, fps int); ScreenSubscriptionID() uint64.
- [x] Write profile clamping, shared output lifecycle, generation isolation tests; observe RED.
- [x] Add WGC output workers sharing native capture, latest raw picture queues and thread ownership.
- [x] Track leased viewer requests, pool matching profiles, target each viewer's request number; preserve base preview/audio.
- [x] Re-send subscriptions and sharing announcements; stop outputs before base capture on stop/disconnect.
- [x] Run unit and real multi-viewer codec/relay tests; verify dimensions, cadence, byte counts and recovery after switching.

### Task 3: Playback controls and delivery

Files: cmd/relayclient/main.go, tests, README.md, tests/manual_test_cases.md.
- [x] Write UI selection and generation-reset tests; observe RED.
- [x] Add resolution/FPS selectors to screen tab; update preferences and selected client immediately.
- [x] Reset only video decoder/order on new request, reject pending old pictures, retain A/V clock.
- [x] Run full suite and live playback; request independent review; resolve material findings.
- [x] Build a distinct Windows client and Windows/Linux servers; copy unlocked standard binary paths and report exact limitations.

## Execution record

Native implementation in existing non-Git project. User explicitly selected transmission with lower bandwidth; reversible implementation is authorized by that request. No additional approval gate is inserted.

Task 1 complete: RAV2 and broadcast filtering RED→GREEN; validation, request ordering and legacy fallback tests pass.
Task 2 complete: profile/worker/generation tests RED→GREEN; actual multi-viewer video 4.29 vs12.17Mbps,177 vs351 frames in six seconds; static switch pass.
Task 3 complete: UI/order tests RED→GREEN; full default suite pass; independent review findings reproduced and fixed (source snapshot, DXGI encoder ownership, older source compatibility/discovery). Final 1080p high-rate probe with GOMAXPROCS=4 pass at99FPS; prior84/59FPS results retained in report.
Delivery: Windows client/server and Linux amd64 server built; all standard targets and client aliases updated after checking no running binaries. Artifact hashes in tests/playback_results_2026-10-02.md.
