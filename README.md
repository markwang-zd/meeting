# Meeting Room Schedule API

将钉钉会议室日程查询封装为内部 HTTP 服务。服务使用 AppKey 与 AppSecret 自动获取并缓存钉钉访问令牌，调用方无需传递凭据。

## 启动

```sh
export DINGTALK_APP_KEY='your-app-key'
export DINGTALK_APP_SECRET='your-app-secret'
export DINGTALK_USER_ID='dashboard-user-union-id'
go run server.go
```

可选环境变量：

- `LISTEN_ADDR`：监听地址，默认 `:8080`。
- `DINGTALK_BASE_URL`：钉钉 API 地址，默认 `https://api.dingtalk.com`；仅测试或私有网关场景需要设置。
- `DASHBOARD_TIME_ZONE`：看板时区，默认 `Asia/Shanghai`。
- `DASHBOARD_REFRESH_INTERVAL`：看板日程缓存刷新间隔，默认 `30s`。支持 Go duration 格式（例如 `1m`）或纯秒数（例如 `300`）。

## 接口

`POST /v1/meeting-rooms/schedules`

```json
{
  "userId": "manager-user-id",
  "roomIds": ["4002xxxxx"],
  "startTime": "2026-10-22T10:00:00+08:00",
  "endTime": "2026-10-22T11:00:00+08:00"
}
```

- `userId`：钉钉用户 ID。
- `roomIds`：会议室 ID 列表，数量为 1 到 6。
- `startTime`、`endTime`：ISO 8601 时间，且结束时间必须晚于开始时间。服务会转换为钉钉要求的 UTC 格式后再请求。

服务直接返回钉钉 API 的 JSON 数据。请求参数错误返回 `400`；钉钉不可访问或钉钉返回非成功状态时返回 `502`。

健康检查：`GET /healthz`。

## 看板页面

打开 `index.html` 时通过 URL 参数传入会议室序号：

```text
index.html?roomId=1
```

序号与钉钉会议室 ID 的映射由服务端维护：

- `1`：稻妻（Inazuma）
- `2`：枫丹（Fontaine）
- `3`：璃月（Liyue）
- `4`：蒙徳（Mondstadt）
- `5`：纳塔（Natlan）
- `6`：须弥（Sumeru）

服务启动时会一次性查询全部 6 个会议室当天的日程，并按 `DASHBOARD_REFRESH_INTERVAL` 定时刷新。最新成功查询结果仅保存在服务进程内；刷新失败时继续保留上一次成功的结果。

页面会请求 `/api/dingtalk/meeting-rooms/{roomId}/status`。服务端将序号转换为真实钉钉会议室 ID，并从本地缓存中计算当前会议与今日后续日程，不会因客户端请求再次调用钉钉。会议室名称和容纳人数不再由该接口查询。缓存尚未准备好时接口返回 `503`；传入 `1` 到 `6` 之外的值时返回 `400`。

请仅将服务暴露在可信内网，或由 API 网关为该接口增加调用方鉴权；该服务本身不接收或校验调用方凭据。
# meeting
