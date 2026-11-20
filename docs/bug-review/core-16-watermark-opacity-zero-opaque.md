# Opacity=0 hiển thị watermark đục hoàn toàn thay vì trong suốt
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** domain, transcoder
- **Vị trí:** internal/domain/watermark.go:76,136,234-235; internal/transcoder/native/watermark.go:225,255

## Hiện tượng
Operator đặt `opacity: 0` để ẩn logo. Logo hiện rõ 100%.

## Điều kiện kích hoạt
Watermark bật với `opacity: 0`, là giá trị hợp lệ theo `Validate` (`[0,1]`).

## Phân tích
```go
if w.Opacity != 0 && w.Opacity < 0.01 { return false }   // IsActive
...
if out.Opacity == 0 { out.Opacity = defaultWatermarkOpacity } // Resolved
```
Do đó 0 được coi là chưa đặt và thành 1.0. Filter cũng chỉ áp dụng khi `0 < Opacity < 1`.

## Vì sao đây là lỗi
Comment của field ghi "0.0 = fully transparent". Giá trị 0.005 thì tắt watermark, nhưng 0 lại đục: hành vi không liên tục.

## Hướng sửa
Dùng `*float64` để phân biệt chưa đặt và 0, hoặc từ chối 0 trong `Validate` và sửa doc.

## Cách xác minh
`(&WatermarkConfig{Enabled:true,Type:"text",Text:"x",Opacity:0}).Resolved().Opacity` hiện trả 1.0. Kiểm tra `IsActive()` phải là false hoặc API phải trả 400.
