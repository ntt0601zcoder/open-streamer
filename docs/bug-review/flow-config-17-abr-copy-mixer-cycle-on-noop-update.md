# Mọi lần lưu đều dựng lại toàn bộ pipeline ABR copy và ABR mixer

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/api/handler, internal/publisher
- **Vị trí:** internal/coordinator/coordinator.go:562-575, internal/api/handler/stream.go:447-451, internal/api/handler/template.go:253-260, internal/api/handler/config_yaml.go:596-599, internal/publisher/service.go:408-424

## Hiện tượng
Đổi `name`, `tags`, `description`, hoặc gửi lại body không đổi trên stream ABR copy làm người xem HLS và DASH bị ngắt: thư mục segment bị xoá và push ra ngoài bị kết nối lại. Mỗi lần `PUT /config/yaml` đều cắt toàn bộ các stream loại này, kể cả khi không sửa gì.

## Điều kiện kích hoạt
Stream ABR copy đang chạy, `POST /streams/{code}` với `{"description":"x"}`.

## Phân tích
Nhánh ABR trong `updateLocked` chạy trước `ComputeDiff` và không có điều kiện về nội dung thay đổi:

```go
if wasABRCopy || wasABRMixer || willBeABRCopy || willBeABRMixer {
    c.stopLocked(ctx, old.Code)
    if new.Disabled || len(new.Inputs) == 0 {
        return nil
    }
    return c.startLocked(ctx, new)
}
```

`Publisher.Stop` chờ goroutine rồi `cleanupStreamDirs`. `applyStreams` gọi `Update` cho mọi stream đang chạy trong tài liệu YAML, không xét có đổi hay không. Với template cũng vậy.

## Vì sao đây là lỗi
Invariant 5 trong CLAUDE.md yêu cầu hot-reload theo diff với tập nhỏ nhất. `TestComputeDiff_MetadataOnly` xác nhận metadata không phải thay đổi cần áp dụng, nhưng nhánh này không qua diff. Trạng thái `upstream` cũng không so sánh.

## Hướng sửa
Khi cả hai phía cùng là ABR copy hoặc mixer và cùng upstream, dùng `ComputeDiff` để bỏ qua update không ảnh hưởng và định tuyến protocols, push, policy qua `UpdateProtocols`. Chỉ full-cycle khi đổi topology hoặc input.

## Cách xác minh
Test coordinator: `Update` ABR copy với `old` và `new` chỉ khác `Description`, kiểm tra `pub.Stop` không được gọi.
