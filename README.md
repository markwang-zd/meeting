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
- `DASHBOARD_REFRESH_INTERVAL`：看板日程缓存刷新间隔，默认 `5m`。支持 Go duration 格式（例如 `1m`）或纯秒数（例如 `300`）。
- `DASHBOARD_REFRESH_WINDOWS`：可选的后台刷新时间段，格式为以逗号分隔的 `HH:MM-HH:MM`，例如 `08:00-12:00,13:00-18:00`。仅在这些时间段内按刷新间隔执行；支持跨午夜时间段（例如 `22:00-02:00`）。未设置时在工作日全天刷新。

## 接口

`POST /v1/meeting-rooms/schedules` 已禁用，返回 `410 Gone`。该接口此前会为每个客户端请求直接调用钉钉，绕过缓存刷新时段。

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

服务仅在看板时区的周一至周五请求钉钉刷新缓存。服务启动时也仅在工作日且处于 `DASHBOARD_REFRESH_WINDOWS` 指定的时间段内才会查询全部 6 个会议室当天的日程；之后按 `DASHBOARD_REFRESH_INTERVAL` 定时刷新。每次刷新只调用一次钉钉会议室日程批量查询接口，不再为事件详情或发起人信息追加请求。日程查询结果仅保存在服务进程内；刷新失败时继续保留上一次成功的结果。

页面请求 `GET /api/dingtalk/meeting-rooms/{roomId}/status`，其中 `{roomId}` 为 URL 中的 `1` 到 `6`。服务端将序号转换为钉钉会议室 ID，并从本地缓存返回该房间的状态和日程；页面请求不会调用钉钉。名称由 `server.go` 中的本地映射维护；尚未提供的容纳人数会返回 `0` 并在页面显示为未知。缓存尚未准备好时接口返回 `503`。

请仅将服务暴露在可信内网，或由 API 网关为该接口增加调用方鉴权；该服务本身不接收或校验调用方凭据。
