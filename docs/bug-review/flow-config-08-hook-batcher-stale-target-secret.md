# Sửa hook HTTP vẫn gửi đến URL cũ với secret cũ

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/hooks, internal/store
- **Vị trí:** internal/hooks/service.go:293-326, internal/hooks/batcher.go:64-73, internal/hooks/batcher.go:458-498, internal/api/handler/hook.go:139-162, internal/api/handler/config_yaml.go:460-467

## Hiện tượng
Operator đổi `target` của hook vì endpoint chuyển chỗ, hoặc đổi `secret` vì bị lộ. `PUT /hooks/{id}` trả 200 và `GET` hiện giá trị mới. Sự kiện vẫn được POST đến URL cũ và ký bằng secret cũ cho tới khi server restart.

## Điều kiện kích hoạt
1. Hook HTTP đã nhận ít nhất một event, nên batcher đã được tạo.
2. `PUT /hooks/{id}` đổi `target` hoặc `secret`, hoặc sửa trong `PUT /config/yaml`.

## Phân tích
`batcherFor` tái sử dụng batcher theo `h.ID` và bỏ qua `h` mới:

```go
b, ok := s.batchers[h.ID]
if ok {
    return b
}
```

`batchConfig` chứa cả `secret` và `target`, được chụp lúc tạo. `dispatch` tuy đọc hook mới từ repo cho `Enabled` và bộ lọc, nhưng việc gửi đi vẫn dùng cấu hình cũ. Handler chỉ lưu repo và không báo gì cho hooks service.

Batcher của hook đã xoá cũng ở lại trong map, tiếp tục retry phần queue còn lại đến target đã bị xoá.

## Vì sao đây là lỗi
Comment trên `batcherFor` chỉ thừa nhận giới hạn với "per-hook batch settings", trong khi cả địa chỉ đích và khoá ký cũng bị đông cứng. Sự kiện tiếp tục rò ra endpoint mà operator đã gỡ bỏ, và secret cũ vẫn là khoá ký.

## Hướng sửa
So `mergeBatchConfig` mới với `b.cfg` trong `batcherFor`: nếu khác, tạo batcher mới và chuyển queue hoặc đổi target và secret an toàn. Khi hook bị xoá hoặc tắt, dừng batcher.

## Cách xác minh
Test với hai `httptest.Server`: dispatch một event, đổi `Target` trong repo, dispatch tiếp, kiểm tra server mới nhận được.
