# 房间投票与房主权限移交

## 用户规则

- 成员可以对其他成员发起投票。针对普通成员：踢出目标并将房主权限移交发起人。针对当前房主：只移交权限，原房主保留成员身份。
- 投票最长 60 秒。发起人自动赞成；目标不参与、不计入分母；发起时其他在线成员形成固定名单，须严格超过半数赞成。后来加入者不参与本轮。
- 达到多数立即进入 30 秒房主否决期。仅明确点击否决取消；关闭窗口、不操作或没有响应均在期限到达后执行。
- 原有房主直接踢人的入口保留。每个房间同时只进行一轮投票。

## 边界与权限

服务器负责投票、超时、权限校验、原子移交。成员只投一次最终票。发起人或目标离开时取消；其他成员离开不修改本轮名单及已投票。过期后不接受补票或否决。

否决人锁定为达到多数时的房主，只有该成员仍为当前房主时可以否决。该房主离开而目标仍在时，不视为否决，原截止时间继续。若目标为离开的房主，本轮取消，原有自动接任仍生效。

管理房主与游戏服务来源分开：移交管理权不搬迁游戏进程，也不打断原房主仍在提供的游戏服务。游戏来源离开时沿用自动接任来源的兼容行为；实际游戏仍需在提供服务的电脑上运行。

## 消息接口（客户端与服务端共用）

新增 `internal/proto/room_vote.go`，不修改已有媒体消息。

```go
const TypeRoomControl byte = 0x15
const TypeRoomState byte = 0x16
// byte constants, values in listed order:
// RoomControlSync=0, RoomControlStart=1, RoomControlYes=2,
// RoomControlNo=3, RoomControlVeto=4
// RoomVoteKick=1, RoomVoteTransfer=2
// RoomVoteNone=0, RoomVoteVoting=1, RoomVoteVeto=2,
// RoomVoteExecuted=3, RoomVoteVetoed=4, RoomVoteExpired=5,
// RoomVoteCancelled=6
type RoomControl struct {
    Action byte
    RequestID uint64
    VoteID uint64
    TargetID uint32
}
type RoomVote struct {
    ID uint64
    Kind, Phase byte
    InitiatorID, TargetID, VetoHostID uint32
    InitiatorName, TargetName string
    Eligible, Yes, No uint16
    RemainingMS uint32
    CanVote bool
    Ballot byte // 0=未投, 1=赞成, 2=反对
}
type RoomState struct {
    Sequence, AckRequestID uint64
    HostID, GameHostID uint32
    Members []Member
    Vote RoomVote
    Error string
}
func EncodeRoomControl(RoomControl) []byte
func DecodeRoomControl([]byte) (RoomControl, error)
func EncodeRoomState(RoomState) []byte
func DecodeRoomState([]byte) (RoomState, error)
```

二进制格式遵循现有大端协议；拒绝损坏、未知动作与截断包。请求号在连接内单调递增，服务端确认并去重。投票编号在服务器内全局递增，房间重建不重用。

客户端每秒同步，重发未确认请求；状态带严格递增序号，旧状态不能覆盖新权限/结果。服务端个人化状态携带资格、已投票、请求确认和错误。被踢成员继续同步时应收到拒绝，补偿首次拒绝包丢失。终态保留 30 秒后清除。

## 客户端接口与界面

`tunnel.Status` 新增 `RoomVotingSupported bool`, `RoomVote proto.RoomVote`, `RoomControlAckID uint64`, `RoomControlError string`, `RoomControlPending bool`，以及 `GameHostID uint32`。返回剩余时间时扣除收到状态后的本地经过时间。

```go
func (*Client) StartRoomVote(targetID uint32) error
func (*Client) CastRoomVote(voteID uint64, agree bool) error
func (*Client) VetoRoomVote(voteID uint64) error
```

右键其他成员发起踢人投票；右键房主显示“投票接任房主”。全局投票条显示人数、倒计时、赞成/反对和结果。达到多数时，房主窗口显示倒计时否决对话框；关闭不等于否决，同一轮只自动弹一次。回调捕获所属连接与投票编号，重新连接后旧窗口无效。

客户端房主身份必须随服务器同步，成员菜单以当前连接角色判定。拒绝连接后的清理不得在读循环里等待自己。

## 验收

严格过半、两人房间、重复/乱序包、超时补票、非房主伪造否决、跨房间命令、成员退出、转交后旧房主留房、管理/游戏来源分离、丢拒绝包恢复、真实中继投票和 Fyne 弹窗/倒计时均须覆盖。Windows 客户端与 Windows/Linux 服务端可构建，保持原媒体协议与播放档位功能。
