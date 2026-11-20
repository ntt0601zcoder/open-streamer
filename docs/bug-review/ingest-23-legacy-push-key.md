# Khoá push từ URL legacy không khớp đường định tuyến RTMP và không có SRT ingest
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor, ingestor/push, publisher, domain
- **Vị trí:** internal/ingestor/service.go:697-707; internal/ingestor/push/rtmp_server.go:66-89; internal/domain/input.go:22-27; pkg/protocol/protocol.go:94-104; internal/publisher/serve_srt.go:90-103

## Hiện tượng
Input legacy `rtmp://0.0.0.0:1935/myapp/cam1` hoặc `rtmp://0.0.0.0:1935` hiển thị slot sẵn sàng nhưng encoder bị từ chối với "no stream registered". Input `srt://0.0.0.0:9999?streamid=key` không thể nhận push.

## Điều kiện kích hoạt
Dùng dạng wildcard legacy mà không phải đúng `.../live/<key>`.

## Phân tích
Khoá lấy theo đoạn cuối chuỗi thô, không qua `url.Parse`:
```go
raw := strings.TrimRight(input.URL, "/")
if idx := strings.LastIndex(raw, "/"); idx >= 0 {
    return raw[idx+1:]
}
```
- `rtmp://0.0.0.0:1935` cho khoá `0.0.0.0:1935`, không encoder nào khớp được, dù `protocol.go` liệt kê dạng này.
- `.../myapp/cam1` cho khoá `cam1`, nhưng `rtmpRouteKey("myapp","cam1")` trả `myapp/cam1`.
- Có query, ví dụ `.../live/k?x=1`, khoá giữ cả `?x=1`.
- `srt://0.0.0.0:9999?streamid=k` cho khoá `0.0.0.0:9999?streamid=k`.

Hơn nữa `serve_srt.go` chỉ trả `srt.SUBSCRIBE`; registry không được SRT dùng, mặc dù `domain/input.go` mô tả "SRT push — our SRT server listens".

Không trùng với lỗi đã biết về loopback: ở đây địa chỉ wildcard được phân loại đúng là push, chỉ khoá sai.

## Vì sao đây là lỗi
Comment hứa "preserving backward compatibility", và tài liệu input nêu các dạng trên. Test `service_test.go` chỉ kiểm tra dạng `.../live/<key>`.

## Hướng sửa
Parse URL, áp cùng quy tắc với `rtmpRouteKey`. Từ chối ở API các URL không tạo được khoá hợp lệ. Sửa tài liệu SRT hoặc từ chối `srt://0.0.0.0`.

## Cách xác minh
Table test `pushStreamKey` so khớp với `rtmpRouteKey` cho mỗi dạng URL.
