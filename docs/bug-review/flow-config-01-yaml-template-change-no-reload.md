# Sửa template qua PUT /config/yaml không đến được stream đang chạy

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/store, internal/domain, internal/coordinator
- **Vị trí:** internal/api/handler/config_yaml.go:194, internal/api/handler/config_yaml.go:213, internal/api/handler/config_yaml.go:329-336, internal/api/handler/config_yaml.go:592-599, internal/api/handler/config_yaml.go:612-621, internal/api/handler/template.go:240-263

## Hiện tượng
Operator đổi ladder, protocols, push hoặc input của một template trong trình soạn YAML. API trả 200 và `GET /config/yaml` hiện giá trị mới, nhưng mọi stream đang chạy kế thừa template đó vẫn dùng cấu hình cũ cho tới khi bị restart.

## Điều kiện kích hoạt
1. Stream A đang chạy, tham chiếu template T và để trống `transcoder`.
2. `PUT /config/yaml` đổi `templates[T].transcoder.video.profiles`, phần `streams[A]` giữ nguyên.

## Phân tích
`ReplaceConfigYAML` gọi `applyTemplates` trước, nên template mới đã nằm trong store:

```go
if err := h.templateRepo.Save(ctx, tpl); err != nil {
```

Sau đó `reconcileStreamLifecycle` resolve cả hai phía bằng cùng template vừa lưu:

```go
resolvedOld := h.resolveStream(ctx, old)
resolvedWant := h.resolveStream(ctx, want)
```

Hai bản merge giống hệt nhau nên `ComputeDiff` rỗng và `coord.Update` không làm gì. `TemplateHandler.Put` tránh được lỗi này vì giữ `prior` và resolve phía cũ theo template cũ (`domain.ResolveStream(s, prior)`). Đường YAML không giữ snapshot đó.

Nếu template cũ bị xoá trong cùng bản sửa, `resolveStream(old)` trả về bản ghi thô, nên diff còn so nhầm bản thô với bản đã merge.

## Vì sao đây là lỗi
Comment trong `reconcileStreamLifecycle` nói mục đích là để diff engine thấy cấu hình kế thừa, nhưng chỉ đúng khi bản thân stream đổi. Reconciler chỉ khởi động stream đã dừng nên không sửa được. `TestReplaceConfigYAMLUpdatesRunningStream` chỉ kiểm tra stream không dùng template.

## Hướng sửa
Trong `ReplaceConfigYAML`, chụp danh sách template cũ trước `applyTemplates` và truyền vào `applyStreams`, rồi resolve `old` theo template cũ.

## Cách xác minh
Unit test với store giả: stream đang chạy tham chiếu template, PUT YAML chỉ đổi template, kiểm tra `coord.Update` nhận `old` khác `new`. Thử thực tế: đổi `protocols.dash` trong template, thấy thư mục DASH không xuất hiện.
