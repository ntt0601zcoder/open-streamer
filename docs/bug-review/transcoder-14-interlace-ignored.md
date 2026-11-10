# video.interlace không được áp dụng, nguồn interlaced không được deinterlace

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/domain, internal/transcoder, internal/transcoder/native
- **Vị trí:** internal/domain/transcoder.go:206-209, internal/transcoder/supervisor.go:536-566, internal/transcoder/native/proto/transcoder.proto:50-57, internal/transcoder/native/stream_pipeline.go:1403-1416

## Hiện tượng
Nguồn broadcast interlaced (1080i) với `video.interlace` đã cấu hình vẫn ra hình có răng lược ở chuyển động, và scale làm lỗi này rõ hơn.

## Điều kiện kích hoạt
Input interlaced và `transcoder.video.interlace` khác rỗng.

## Phân tích
Comment domain khẳng định: "Interlace selects the deinterlace pre-filter applied before scaling. Applies in the transcoder's video pipeline". Nhưng `grep Interlace` trong `internal/` không có nơi sử dụng nào ngoài khai báo. `buildConfigure` không gửi field này, `ConfigureRequest` và `Target` không có field tương ứng, và đường frame đi thẳng từ decoder vào scaler, encoder.

## Vì sao đây là lỗi
Cấu hình được API chấp nhận và lưu nhưng không có tác dụng và không có cảnh báo. Không có filter deinterlace nào trong `Scaler`, `gpuScaler` hay `Watermarker`.

## Hướng sửa
Thêm field vào proto và chèn deinterlace trước scale (`yadif` trên CPU, `yadif_cuda` hoặc chế độ deinterlace của cuvid trên GPU). Nếu chưa hỗ trợ thì từ chối ở validate.

## Cách xác minh
Transcode nguồn interlaced mẫu có và không có `interlace`, so sánh output. Kiểm tra `buildConfigure` có mang giá trị này hay không.
