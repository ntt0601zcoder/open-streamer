# /config/defaults quảng bá port mặc định mà listener không áp dụng

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/runtime, internal/ingestor, internal/publisher, config
- **Vị trí:** internal/api/handler/config_defaults.go:208-245, internal/runtime/manager.go:373-385, internal/ingestor/service.go:232-240, internal/publisher/serve_rtsp.go:128-132, internal/publisher/serve_srt.go:41-44, config/config.go (RTMPListenerConfig.Port, RTSPListenerConfig.Port, SRTListenerConfig.Port)

## Hiện tượng
UI dùng `/config/defaults` làm placeholder và hiện port 1935. Operator bật RTMP, để trống port và lưu qua `POST /config`. API trả 200 nhưng listener không mở, encoder bị từ chối kết nối, không có log lỗi.

## Điều kiện kích hoạt
`POST /config` với `{"listeners":{"rtmp":{"enabled":true}}}`.

## Phân tích
`buildConfigDefaults` báo `rtmpPort = 1935`, `rtspPort = 554`, `srtPort = 9999`, và comment trong struct cũng ghi `// default 1935`. Runtime không có mặc định nào:

```go
func rtmpListenerEnabled(l *config.ListenersConfig) bool {
    return l != nil && l.RTMP.Enabled && l.RTMP.Port > 0
}
```

Đường YAML từ chối port 0 khi enabled ("required when listener is enabled"), nhưng `POST /config` không validate.

## Vì sao đây là lỗi
Comment trong `configDefaultsResponse` định nghĩa endpoint này là "the implicit value the server applies", còn với port thì server không áp dụng giá trị nào. Hai đường API cũng xử lý khác nhau.

## Hướng sửa
Hoặc áp dụng port mặc định trong runtime và các listener, hoặc bỏ port khỏi `/config/defaults` và trả lỗi validate ở `POST /config`.

## Cách xác minh
Test `rtmpListenerEnabled` với `Enabled:true, Port:0` so với giá trị `/config/defaults`. Thử thực tế trên instance riêng.
