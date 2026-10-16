# LAN Room

Windows-only desktop client with Windows and Linux server targets. The server can provision isolated virtual LAN rooms through SoftEther VPN Server. Voice and screen media still require a separate media service.

## Current status

- Cross-platform Go server executable.
- Windows WPF desktop client.
- Set a room ID and password when creating a room, or leave either field empty to generate it automatically. Room IDs are unique regardless of letter case.
- Join a room with its ID and password.
- Real-time room presence and text chat over WebSocket.
- Room and member state is saved to `lanroom-state.json`; recent chat messages remain in memory and clear when the server exits.
- With SoftEther enabled, each room gets a Virtual Hub, an isolated DHCP subnet, and a separate VPN account for each member. The room create/join response contains that member's VPN connection details.

The full requirements baseline is in [PROJECT_SPEC.md](PROJECT_SPEC.md). Server setup for both platforms is in [server/DEPLOY.md](server/DEPLOY.md).

## Run the server

Requires Go 1.24 or newer.

```powershell
cd server
$env:LANROOM_CHAT_ONLY = "1" # local room/chat development only
go run ./cmd/lanroom-server
```

The server requires SoftEther configuration for virtual networking. `LANROOM_CHAT_ONLY=1` explicitly starts a room/chat development server without VPN. The server listens on `:8080` by default. Set `LANROOM_ADDR` to change the listen address, for example `127.0.0.1:8080` for local development. `LANROOM_STATE_FILE` changes the state file path. The state file contains member tokens and salted password hashes; keep it private and back it up.

Build for the current platform:

```powershell
cd server
go build -o lanroom-server.exe ./cmd/lanroom-server
```

Build a Linux x64 server binary from Windows:

```powershell
cd server
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o lanroom-server ./cmd/lanroom-server
Remove-Item Env:GOOS
Remove-Item Env:GOARCH
```

## Run the Windows desktop client

Requires the .NET 9 SDK and Windows Desktop Runtime.

```powershell
dotnet run --project client/LanRoom.Client/LanRoom.Client.csproj
```

The client currently supports room creation with a custom ID/password, password-based joining, member presence, and text chat. Enter the server's reachable HTTP address in the connection screen. When the server provisions a VPN account, the room view can copy its connection details for manual use with SoftEther VPN Client. Automatic VPN connection, voice, and screen sharing are not implemented yet.

## API: create a room

`POST /api/rooms`

```json
{
  "name": "Friday game",
  "room_id": "friday-game",
  "password": "a-private-password",
  "display_name": "Alice"
}
```

`room_id` and `password` are optional; the server generates either one when omitted or empty. IDs use 4–32 ASCII letters, digits, hyphens, or underscores. Passwords use 6–128 Unicode characters without control characters. An existing ID returns HTTP 409. The response includes `room_id`, `room_name`, `password` (shown only when created), `member_token`, `member`, and `websocket_path`. When SoftEther is enabled it also includes `vpn` with `server`, `port`, `hub`, `username`, `password`, and `subnet`. Share the room ID and room password with the people joining. Keep the member token and VPN password private.

## API: join a room

`POST /api/rooms/{room_id}/join`

```json
{
  "password": "a-private-password",
  "display_name": "Bob"
}
```

The response includes that member's private `member_token` and the WebSocket path; it does not repeat the room password. Existing rooms created by the older invite-code API accept their invite code as the password.

## WebSocket: presence and chat

Connect to `GET /api/rooms/{room_id}/ws` with `Authorization: Bearer <member_token>`.

On connection, the server sends `room.snapshot` with currently online members and recent messages. It then sends `member.joined`, `member.left`, and `chat.message` events as room state changes.

Send chat as a text frame:

```json
{
  "type": "chat.send",
  "text": "Hello"
}
```

The server retains the last 100 messages in memory per room. Chat text is limited to 2,000 Unicode characters.

## Development boundary

This is still a development foundation, not a public deployment package. The Windows client does not yet connect its virtual adapter automatically; it currently handles room and text chat only. Voice, screen share, computer audio, account management, room moderation, and media forwarding are pending. Provisioning has not been verified against a live SoftEther installation here. The API has no room creation authorization or rate limit yet, so keep it on a trusted network during development. Use HTTPS/WSS through a properly configured reverse proxy before public exposure.
