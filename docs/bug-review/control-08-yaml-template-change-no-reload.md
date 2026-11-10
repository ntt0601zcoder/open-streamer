# Sửa template qua YAML editor không cập nhật stream đang chạy
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** api/handler, coordinator
- **Vị trí:** internal/api/handler/config_yaml.go:194, 213, 329-336, 592-597; internal/api/handler/template.go:256-258

## Hiện tượng
Operator đổi template trong `PUT /config/yaml`, ví dụ thêm rendition hoặc tắt HLS. API trả 200, nhưng các stream đang chạy và kế thừa template vẫn dùng cấu hình cũ cho tới khi restart. Sửa cùng nội dung đó qua `POST /templates/{code}` thì có tác dụng.

## Điều kiện kích hoạt
`PUT /config/yaml` đổi một trường của template, còn phần `streams` của các stream kế thừa giữ nguyên.

## Phân tích
`applyTemplates` lưu template mới trước `applyStreams`. Khi tới `reconcileStreamLifecycle`, cả hai phía đều được resolve với template đã là bản mới:
```go
resolvedOld := h.resolveStream(ctx, old)
resolvedWant := h.resolveStream(ctx, want)
```
`old` và `want` giống nhau ở các trường kế thừa, nên `ComputeDiff(resolvedOld, resolvedWant)` không thấy thay đổi và `Update` không làm gì.

## Vì sao đây là lỗi
`TemplateHandler.reloadStreams` giải quyết đúng bằng cách resolve `old` với template trước khi lưu (`prior`) và `new` với template sau khi lưu. Luồng YAML thiếu snapshot template cũ. Comment ngay trong hàm này giải thích vì sao phải resolve trước khi diff, nhưng thứ tự apply đã làm mất tác dụng.

## Hướng sửa
Lấy snapshot template trước `applyTemplates`, rồi dùng nó để resolve `old` trong `reconcileStreamLifecycle`. Không đổi thứ tự apply, vì stream cần template mới khi start.

## Cách xác minh
Test handler với repo giả: stream kế thừa `Protocols.HLS=true`, YAML đổi template thành `HLS=false`. Kiểm tra `coord.Update` nhận `old.Protocols.HLS=true` và `new.Protocols.HLS=false`. Hiện tại cả hai đều `false`.
