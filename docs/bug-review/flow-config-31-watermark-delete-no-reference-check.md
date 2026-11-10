# Xoá watermark đang được dùng không bị chặn, stream mất logo âm thầm

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/api/handler, internal/watermarks, internal/coordinator, internal/domain, internal/transcoder/native
- **Vị trí:** internal/api/handler/watermark.go:217-229, internal/coordinator/coordinator.go:1042-1059, internal/domain/watermark.go:171-175, internal/api/handler/policy.go:189-202, internal/api/handler/template.go:208-224, internal/transcoder/supervisor.go:677, internal/transcoder/native/watermark.go:250-254

## Hiện tượng
Operator xoá một logo trong thư viện trong khi stream hoặc template vẫn tham chiếu tới. API trả 204. Lần reload hoặc restart tiếp theo, stream phát không có logo. Với kênh bắt buộc đóng dấu thì đây là sự cố nội dung mà chỉ có một log Warn.

## Điều kiện kích hoạt
1. Stream transcode có `watermark: {type: image, filename: logo.png}`.
2. `DELETE /watermarks/logo.png`.
3. `POST /streams/{code}/restart`, hoặc đổi ladder, hoặc server restart.

## Phân tích
Handler xoá thẳng mà không tìm tham chiếu, khác với policy (409 `POLICY_IN_USE`) và template (409 `TEMPLATE_IN_USE`):

```go
fn := domain.WatermarkFilename(name)
if err := h.svc.Delete(fn); err != nil {
```

Ở lần start kế tiếp, coordinator nuốt lỗi:

```go
path, err := c.wmAssets.ResolvePath(wm.Filename)
if err != nil {
    slog.Warn("coordinator: watermark asset resolve failed", ...)
    return nil
}
```

`Watermark.Validate` chỉ kiểm tra định dạng tên, nên lưu stream trỏ tới file không tồn tại cũng được chấp nhận.

Khi stream đang chạy và subprocess bị crash rồi respawn, supervisor vẫn gửi `AssetPath` cũ tới file đã xoá; `movie=` trong filter graph có thể lỗi và gây vòng crash (chưa chạy thực tế, đây là phần làm giảm độ tin cậy).

## Vì sao đây là lỗi
Cùng là tham chiếu giữa các tài nguyên cấu hình nhưng không được bảo vệ như hai loại còn lại, và lỗi bị chuyển thành hành vi im lặng.

## Hướng sửa
Trong `WatermarkHandler.Delete`, quét stream và template, trả 409 kèm danh sách tham chiếu. Khi lưu, kiểm tra asset tồn tại.

## Cách xác minh
Test handler với repo có stream tham chiếu asset, mong đợi 409.
