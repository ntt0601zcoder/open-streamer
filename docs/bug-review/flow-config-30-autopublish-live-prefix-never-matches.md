# Prefix auto-publish "live" như tài liệu không bao giờ khớp

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/ingestor/push, internal/autopublish, internal/domain, internal/api/handler
- **Vị trí:** internal/ingestor/push/rtmp_server.go:66-89, internal/ingestor/push/rtmp_server.go:323, internal/autopublish/service.go:151, internal/autopublish/service.go:167-170, internal/domain/template.go:176-186, docs/USER_GUIDE.md:370, docs/USER_GUIDE.md:376-378, docs/USER_GUIDE.md:396, docs/CONFIG.md:482, internal/autopublish/service_test.go:138

## Hiện tượng
Operator làm đúng ví dụ trong tài liệu: template `"prefixes": ["live"]` với `publish://`, rồi encoder đẩy `rtmp://host:1935/live/foo`. Encoder bị từ chối, stream không được tạo. Lý do chỉ có ở log Debug ("auto-publish resolver declined").

## Điều kiện kích hoạt
1. `POST /templates/auto_news` với `{"prefixes":["live"],"inputs":[{"url":"publish://"}],"protocols":{"hls":true}}`.
2. Đẩy RTMP tới `rtmp://host:1935/live/foo`, hoặc `live/foo/bar`.

## Phân tích
RTMP server luôn bỏ `live/` trước khi tra cứu:

```go
hadLivePrefix := strings.HasPrefix(combined, "live/")
combined = strings.TrimPrefix(combined, "live/")
```

Nên key là `foo` hoặc `foo/bar`, rồi `resolver.ResolveOrCreate(ctx, key, secret)`. `Match("foo")` so với prefix `live`:

```go
if path == prefix { return true }
return strings.HasPrefix(path, prefix+"/")
```

Kết quả là `ErrNoMatch`. Muốn khớp phải đẩy `rtmp://host/live/live/foo`, tạo ra code `live/foo`. Prefix bắt đầu bằng `live/` cũng tương tự.

Test không phát hiện vì gọi thẳng `ResolveOrCreate(ctx, "live/foo", "")`, bỏ qua `rtmpRouteKey`, còn test của route key thì khẳng định kết quả `foo`.

## Vì sao đây là lỗi
Hai module đều đúng khi xét riêng nhưng sai khi ghép lại. `ValidatePrefix` chấp nhận `live`, docs/USER_GUIDE.md còn nói stream sẽ có code `live/foo`. Prefix khác như `region/north` vẫn hoạt động, nên lỗi chỉ xuất hiện với đúng cấu hình mẫu.

## Hướng sửa
Chọn một quy ước. Hoặc truyền đường dẫn gốc chưa bỏ `live/` vào `ResolveOrCreate` và dùng nó làm code (phải đồng bộ với đường phát, vốn cũng bỏ `live/`). Hoặc từ chối prefix `live` và `live/...` lúc lưu, đồng thời sửa tài liệu và ví dụ.

## Cách xác minh
Test ghép: dựng key bằng `rtmpRouteKey("live","foo")` rồi gọi `ResolveOrCreate` với template prefix `live`, hiện trả `ErrNoMatch`. Thử thực tế bằng ffmpeg trên một instance riêng.
