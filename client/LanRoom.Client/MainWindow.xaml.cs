using System.Collections.ObjectModel;
using System.IO;
using System.Net.Http;
using System.Net.Http.Json;
using System.Net.WebSockets;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Windows;
using System.Windows.Input;

namespace LanRoom.Client;

public partial class MainWindow : Window
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);
    private readonly HttpClient _httpClient = new() { Timeout = TimeSpan.FromSeconds(90) };
    private readonly ObservableCollection<string> _members = [];
    private readonly ObservableCollection<string> _messages = [];
    private readonly CancellationTokenSource _lifetime = new();
    private readonly Dictionary<string, string> _memberNames = new(StringComparer.Ordinal);
    private ClientWebSocket? _socket;
    private CancellationTokenSource? _roomCancellation;
    private RoomSession? _session;
    private string _serverAddress = "";

    public MainWindow()
    {
        InitializeComponent();
        MembersList.ItemsSource = _members;
        MessagesList.ItemsSource = _messages;
    }

    private async void CreateRoom_Click(object sender, RoutedEventArgs e)
    {
        var input = new CreateRoomRequest(RoomNameBox.Text.Trim(), CreateRoomIdBox.Text.Trim(), CreatePasswordBox.Password, DisplayNameBox.Text.Trim());
        await RunConnectionAction(async () =>
        {
            var result = await PostAsync<CreateRoomRequest, RoomResponse>("/api/rooms", input);
            await EnterRoomAsync(result, isOwner: true);
        });
    }

    private async void JoinRoom_Click(object sender, RoutedEventArgs e)
    {
        var roomId = RoomIdBox.Text.Trim();
        var input = new JoinRoomRequest(JoinPasswordBox.Password, DisplayNameBox.Text.Trim());
        await RunConnectionAction(async () =>
        {
            var result = await PostAsync<JoinRoomRequest, RoomResponse>($"/api/rooms/{Uri.EscapeDataString(roomId)}/join", input);
            await EnterRoomAsync(result, isOwner: false);
        });
    }

    private async Task RunConnectionAction(Func<Task> action)
    {
        SetBusy(true);
        try
        {
            _serverAddress = NormalizeServerAddress(ServerAddressBox.Text);
            await action();
        }
        catch (HttpRequestException ex)
        {
            SetStatus($"连接服务器失败：{ex.Message}");
        }
        catch (TaskCanceledException)
        {
            SetStatus("请求超时，请检查服务器地址和网络。");
        }
        catch (Exception ex)
        {
            SetStatus($"操作失败：{ex.Message}");
        }
        finally
        {
            SetBusy(false);
        }
    }

    private async Task<TResponse> PostAsync<TRequest, TResponse>(string path, TRequest body)
    {
        using var response = await _httpClient.PostAsJsonAsync(new Uri(new Uri(_serverAddress), path), body, JsonOptions, _lifetime.Token);
        if (!response.IsSuccessStatusCode)
        {
            var errorText = await response.Content.ReadAsStringAsync(_lifetime.Token);
            var message = TryReadApiError(errorText) ?? $"服务器返回 {(int)response.StatusCode} {response.ReasonPhrase}";
            throw new InvalidOperationException(message);
        }

        var result = await response.Content.ReadFromJsonAsync<TResponse>(JsonOptions, _lifetime.Token);
        return result ?? throw new InvalidOperationException("服务器返回了空响应。");
    }

    private async Task EnterRoomAsync(RoomResponse response, bool isOwner)
    {
        await CloseRoomSocketAsync();
        var session = new RoomSession(
            response.RoomId,
            response.RoomName,
            response.Password ?? "",
            response.MemberToken,
            response.Member.Name,
            response.WebSocketPath,
            isOwner,
            response.VPN);
        _session = session;

        RoomTitleText.Text = session.RoomName;
        RoomInfoText.Text = $"房间 ID：{session.RoomId}";
        InviteDetailsText.Text = isOwner
            ? $"房间 ID：{session.RoomId}    密码已设置，可复制房间信息"
            : $"房间 ID：{session.RoomId}    你已加入房间";
        VpnDetailsText.Text = session.VPN is null
            ? "房间网络：服务端未启用 VPN"
            : $"房间网络：{session.VPN.Server}:{session.VPN.Port} · {session.VPN.Hub} · {session.VPN.Subnet}";
        CopyVpnButton.IsEnabled = session.VPN is not null;
        _members.Clear();
        _messages.Clear();
        _memberNames.Clear();

        _roomCancellation = CancellationTokenSource.CreateLinkedTokenSource(_lifetime.Token);
        var socket = new ClientWebSocket();
        socket.Options.SetRequestHeader("Authorization", $"Bearer {session.MemberToken}");
        _socket = socket;
        var socketUri = new Uri(new Uri(_serverAddress), session.WebSocketPath);
        var builder = new UriBuilder(socketUri)
        {
            Scheme = socketUri.Scheme.Equals("https", StringComparison.OrdinalIgnoreCase) ? "wss" : "ws",
            Port = socketUri.IsDefaultPort ? -1 : socketUri.Port,
        };

        try
        {
            await socket.ConnectAsync(builder.Uri, _roomCancellation.Token);
        }
        catch
        {
            await CloseRoomSocketAsync();
            _session = null;
            throw;
        }

        ConnectPanel.Visibility = Visibility.Collapsed;
        RoomPanel.Visibility = Visibility.Visible;
        SetStatus(session.VPN is null
            ? "已连接房间。服务端处于纯聊天模式；远程组网需要在服务端配置 SoftEther VPN。"
            : "已连接房间。可复制 VPN 凭据，在 SoftEther VPN Client 中手动连接；应用内自动连接尚未接入。");
        _ = ReadRoomEventsAsync(socket, _roomCancellation.Token);
    }

    private async Task ReadRoomEventsAsync(ClientWebSocket socket, CancellationToken cancellationToken)
    {
        var buffer = new byte[4096];
        try
        {
            while (socket.State == WebSocketState.Open && !cancellationToken.IsCancellationRequested)
            {
                using var message = new MemoryStream();
                ValueWebSocketReceiveResult result;
                do
                {
                    result = await socket.ReceiveAsync(buffer.AsMemory(), cancellationToken);
                    if (result.MessageType == WebSocketMessageType.Close)
                    {
                        return;
                    }
                    message.Write(buffer, 0, result.Count);
                    if (message.Length > 64 * 1024)
                    {
                        throw new InvalidOperationException("服务器事件超过允许大小。");
                    }
                } while (!result.EndOfMessage);

                if (result.MessageType == WebSocketMessageType.Text)
                {
                    await HandleServerEventAsync(message.ToArray());
                }
            }
        }
        catch (OperationCanceledException)
        {
        }
        catch (WebSocketException ex)
        {
            await Dispatcher.InvokeAsync(() => SetStatus($"房间连接中断：{ex.Message}"));
        }
        catch (Exception ex)
        {
            await Dispatcher.InvokeAsync(() => SetStatus($"接收房间消息失败：{ex.Message}"));
        }
    }

    private async Task HandleServerEventAsync(byte[] payload)
    {
        using var document = JsonDocument.Parse(payload);
        var root = document.RootElement;
        var type = root.GetProperty("type").GetString();
        var data = root.GetProperty("data");

        await Dispatcher.InvokeAsync(() =>
        {
            switch (type)
            {
                case "room.snapshot":
                    ReplaceMembers(data.GetProperty("members"));
                    if (data.TryGetProperty("history", out var history) && history.ValueKind == JsonValueKind.Array)
                    {
                        foreach (var item in history.EnumerateArray())
                        {
                            AddMessage(item.GetProperty("name").GetString(), item.GetProperty("text").GetString(), item.GetProperty("created_at").GetDateTimeOffset());
                        }
                    }
                    break;
                case "member.joined":
                    AddMember(data.GetProperty("id").GetString(), data.GetProperty("name").GetString());
                    break;
                case "member.left":
                    RemoveMember(data.GetProperty("member_id").GetString());
                    break;
                case "chat.message":
                    AddMessage(data.GetProperty("name").GetString(), data.GetProperty("text").GetString(), data.GetProperty("created_at").GetDateTimeOffset());
                    break;
                case "server.error":
                    SetStatus(data.TryGetProperty("message", out var error) ? error.GetString() ?? "服务器错误" : "服务器错误");
                    break;
            }
        });
    }

    private void ReplaceMembers(JsonElement members)
    {
        _members.Clear();
        _memberNames.Clear();
        if (members.ValueKind == JsonValueKind.Array)
        {
            foreach (var member in members.EnumerateArray())
            {
                AddMember(member.GetProperty("id").GetString(), member.GetProperty("name").GetString());
            }
        }
        UpdateMemberCount();
    }

    private void AddMember(string? id, string? name)
    {
        if (string.IsNullOrWhiteSpace(id) || string.IsNullOrWhiteSpace(name) || _memberNames.ContainsKey(id))
        {
            return;
        }
        _memberNames[id] = name;
        _members.Add(name);
        UpdateMemberCount();
    }

    private void RemoveMember(string? id)
    {
        if (string.IsNullOrWhiteSpace(id) || !_memberNames.Remove(id, out var name))
        {
            return;
        }
        _members.Remove(name);
        UpdateMemberCount();
    }

    private void UpdateMemberCount() => MemberCountText.Text = $"房间成员 · {_members.Count}";

    private void AddMessage(string? name, string? text, DateTimeOffset timestamp)
    {
        _messages.Add($"{timestamp.ToLocalTime():HH:mm:ss}  {name ?? "成员"}\n{text ?? ""}");
        while (_messages.Count > 200)
        {
            _messages.RemoveAt(0);
        }
        if (_messages.Count > 0)
        {
            MessagesList.ScrollIntoView(_messages[^1]);
        }
    }

    private async void SendChat_Click(object sender, RoutedEventArgs e) => await SendChatAsync();

    private async void ChatInput_KeyDown(object sender, KeyEventArgs e)
    {
        if (e.Key == Key.Enter && Keyboard.Modifiers == ModifierKeys.None)
        {
            e.Handled = true;
            await SendChatAsync();
        }
    }

    private async Task SendChatAsync()
    {
        var text = ChatInputBox.Text.Trim();
        if (text.Length == 0 || _socket?.State != WebSocketState.Open)
        {
            return;
        }
        var payload = JsonSerializer.SerializeToUtf8Bytes(new ChatSendEvent("chat.send", text), JsonOptions);
        try
        {
            await _socket.SendAsync(payload, WebSocketMessageType.Text, true, _roomCancellation?.Token ?? _lifetime.Token);
            ChatInputBox.Clear();
        }
        catch (Exception ex)
        {
            SetStatus($"发送失败：{ex.Message}");
        }
    }

    private async void CopyInvite_Click(object sender, RoutedEventArgs e)
    {
        if (_session is null)
        {
            return;
        }
        var text = string.IsNullOrEmpty(_session.Password)
            ? $"房间 ID：{_session.RoomId}"
            : $"房间 ID：{_session.RoomId}\n房间密码：{_session.Password}";
        try
        {
            Clipboard.SetText(text);
            SetStatus("房间信息已复制，请只发给要加入的人。");
        }
        catch (Exception ex)
        {
            SetStatus($"复制失败：{ex.Message}");
        }
        await Task.CompletedTask;
    }

    private void CopyVpn_Click(object sender, RoutedEventArgs e)
    {
        if (_session?.VPN is not { } vpn)
        {
            return;
        }
        try
        {
            Clipboard.SetText($"服务器：{vpn.Server}:{vpn.Port}\n虚拟 Hub：{vpn.Hub}\n用户名：{vpn.Username}\n密码：{vpn.Password}\n房间子网：{vpn.Subnet}");
            SetStatus("VPN 连接信息已复制，请勿公开其中的密码。");
        }
        catch (Exception ex)
        {
            SetStatus($"复制失败：{ex.Message}");
        }
    }

    private async void LeaveRoom_Click(object sender, RoutedEventArgs e)
    {
        await CloseRoomSocketAsync();
        _session = null;
        RoomPanel.Visibility = Visibility.Collapsed;
        ConnectPanel.Visibility = Visibility.Visible;
        SetStatus("已离开房间。");
    }

    private async Task CloseRoomSocketAsync()
    {
        _roomCancellation?.Cancel();
        if (_socket is { State: WebSocketState.Open or WebSocketState.CloseReceived })
        {
            try
            {
                using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(2));
                await _socket.CloseOutputAsync(WebSocketCloseStatus.NormalClosure, "leave room", timeout.Token);
            }
            catch
            {
                _socket.Abort();
            }
        }
        _socket?.Dispose();
        _socket = null;
        _roomCancellation?.Dispose();
        _roomCancellation = null;
    }

    private void SetBusy(bool busy)
    {
        CreateRoomButton.IsEnabled = !busy;
        JoinRoomButton.IsEnabled = !busy;
        ServerAddressBox.IsEnabled = !busy;
        DisplayNameBox.IsEnabled = !busy;
        CreateRoomIdBox.IsEnabled = !busy;
        CreatePasswordBox.IsEnabled = !busy;
        JoinPasswordBox.IsEnabled = !busy;
    }

    private void SetStatus(string message) => StatusText.Text = message;

    private static string NormalizeServerAddress(string value)
    {
        var address = value.Trim();
        if (address.Length == 0)
        {
            throw new InvalidOperationException("请填写服务器地址。");
        }
        if (!address.Contains("://", StringComparison.Ordinal))
        {
            address = "http://" + address;
        }
        if (!Uri.TryCreate(address, UriKind.Absolute, out var uri) || (uri.Scheme != "http" && uri.Scheme != "https"))
        {
            throw new InvalidOperationException("服务器地址必须使用 http:// 或 https://。");
        }
        return uri.ToString().TrimEnd('/') + "/";
    }

    private static string? TryReadApiError(string json)
    {
        try
        {
            using var document = JsonDocument.Parse(json);
            if (document.RootElement.TryGetProperty("message", out var message))
            {
                return message.GetString();
            }
        }
        catch (JsonException)
        {
        }
        return null;
    }

    private async void Window_Closing(object? sender, System.ComponentModel.CancelEventArgs e)
    {
        _lifetime.Cancel();
        await CloseRoomSocketAsync();
        _httpClient.Dispose();
        _lifetime.Dispose();
    }

    private sealed record RoomSession(string RoomId, string RoomName, string Password, string MemberToken, string DisplayName, string WebSocketPath, bool IsOwner, VPNResponse? VPN);
    private sealed record CreateRoomRequest(
        [property: JsonPropertyName("name")] string Name,
        [property: JsonPropertyName("room_id")] string RoomId,
        [property: JsonPropertyName("password")] string Password,
        [property: JsonPropertyName("display_name")] string DisplayName);
    private sealed record JoinRoomRequest(
        [property: JsonPropertyName("password")] string Password,
        [property: JsonPropertyName("display_name")] string DisplayName);
    private sealed record MemberResponse(
        [property: JsonPropertyName("id")] string Id,
        [property: JsonPropertyName("name")] string Name);
    private sealed record RoomResponse(
        [property: JsonPropertyName("room_id")] string RoomId,
        [property: JsonPropertyName("room_name")] string RoomName,
        [property: JsonPropertyName("password")] string? Password,
        [property: JsonPropertyName("member_token")] string MemberToken,
        [property: JsonPropertyName("member")] MemberResponse Member,
        [property: JsonPropertyName("websocket_path")] string WebSocketPath,
        [property: JsonPropertyName("vpn")] VPNResponse? VPN);
    private sealed record VPNResponse(
        [property: JsonPropertyName("server")] string Server,
        [property: JsonPropertyName("port")] int Port,
        [property: JsonPropertyName("hub")] string Hub,
        [property: JsonPropertyName("username")] string Username,
        [property: JsonPropertyName("password")] string Password,
        [property: JsonPropertyName("subnet")] string Subnet);
    private sealed record ChatSendEvent(
        [property: JsonPropertyName("type")] string Type,
        [property: JsonPropertyName("text")] string Text);
}
