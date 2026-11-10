# PUT /config/yaml không phát các event chi tiết như tài liệu mô tả

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/events, internal/hooks
- **Vị trí:** internal/api/handler/config_yaml.go:231-242, internal/api/handler/config_yaml.go:324-335, internal/api/handler/config_yaml.go:404-415, internal/api/handler/config_yaml.go:453-467, internal/api/handler/config_yaml.go:546-576, docs/EVENTS.md:113-115

## Hiện tượng
Stream, template, policy hoặc hook được tạo, sửa, xoá qua trình soạn YAML không tạo các event `stream.created`, `stream.deleted`, `template.*`, `policy.*`, `hook.*`. Hệ thống đồng bộ inventory dựa vào event sẽ lệch: stream đã bị xoá vẫn còn trong hệ thống ngoài.

## Điều kiện kích hoạt
Hook nhận `stream.deleted`. `PUT /config/yaml` bỏ một stream khỏi danh sách.

## Phân tích
Trong `config_yaml.go` chỉ có một lời gọi `Publish` duy nhất, cho `EventConfigChanged`. `removeStreams` chỉ `Stop` và `Delete`. `upsertStreams`, `applyHooks`, `applyTemplates`, `applyPolicies` đều không có bus. Comment lại khẳng định:

```go
// individual stream.updated/hook.updated entries. The fine-grained events
// are still emitted by their respective Apply paths.
```

docs/EVENTS.md cũng ghi việc này. Event `stream.stopped` từ coordinator vẫn có, nhưng không phân biệt được với một lần dừng thường.

`EventConfigChanged` hiện cũng không tới được hook, xem `flow-config-07-hooks-missing-event-subscriptions.md`.

## Vì sao đây là lỗi
Comment và tài liệu mô tả hành vi không tồn tại, và cùng một thao tác có event hay không tuỳ đường API.

## Hướng sửa
Phát event tương ứng trong từng bước apply. Có thể tách các hàm publish đang nằm trong các handler REST để dùng chung.

## Cách xác minh
Test `ReplaceConfigYAML` với bus giả: xoá một stream và thêm một hook, kiểm tra có `stream.deleted` và `hook.created`.
