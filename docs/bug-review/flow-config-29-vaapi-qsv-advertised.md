# VAAPI và QSV được API quảng bá và chấp nhận nhưng pipeline không có đường chạy

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/hwdetect, internal/api/handler, internal/domain, internal/transcoder, internal/transcoder/native
- **Vị trí:** internal/hwdetect/detect.go:33-38, internal/api/handler/config.go:142, internal/api/handler/config_defaults.go:158-164, internal/domain/resolvers.go:44-48, internal/transcoder/supervisor.go:640-643, internal/transcoder/native/server.go:392-395, internal/transcoder/native/encoder.go:103-111, internal/transcoder/native/cuda.go:39-43

## Hiện tượng
Trên máy có `/dev/dri`, `GET /config` liệt kê `vaapi` và có thể `qsv`. Operator chọn `global.hw: vaapi`, API lưu thành công, stream rơi vào vòng crash của transcoder và không có đầu ra ABR, trạng thái Degraded.

## Điều kiện kích hoạt
Máy Linux có GPU Intel hoặc AMD, stream có `transcoder.global.hw: vaapi` hoặc `qsv` và ít nhất một profile.

## Phân tích
`hwdetect.Available` thêm `HWAccelVAAPI` chỉ vì có DRI device. `ResolveVideoEncoder` chuyển thành `h264_vaapi` hoặc `h264_qsv`. Trong native chỉ có đường GPU cho CUDA. `NewEncoder` luôn đặt pixel format CPU, trừ NVENC:

```go
gpuInput := cfg.cuda != nil && cfg.cuda.dev != nil && isNVENCEncoder(codecName)
if gpuInput {
    cc.SetPixelFormat(astiav.PixelFormatCuda)
} else {
    cc.SetPixelFormat(astiav.PixelFormatYuv420P)
}
```

Không có chỗ nào tạo hardware device hay hw_frames cho VAAPI hoặc QSV. Encoder `h264_vaapi` cần frame VAAPI, nên `avcodec_open2` với YUV420P dự kiến thất bại. Nếu libav không được build kèm encoder này, `FindEncoderByName` trả nil và cũng lỗi. Không có fallback encoder về CPU, chỉ decoder có.

Độ tin cậy Trung bình vì chưa chạy thực tế trên phần cứng (quy tắc review không cho chạy).

## Vì sao đây là lỗi
`hw_accels` được mô tả là capability thật của host, và không có validate nào đối chiếu với khả năng của pipeline. Khác với issue #52 vì đây là backend phần cứng được quảng bá.

## Hướng sửa
Chỉ liệt kê backend mà native thực sự hỗ trợ, và kiểm tra `global.hw` ở lúc lưu. Nếu muốn hỗ trợ thì thêm device và hw_frames, hoặc fallback về CPU kèm cảnh báo.

## Cách xác minh
Trên máy thử có VAAPI (không phải máy đang phát), tạo stream với `hw: vaapi` và xem log lỗi mở encoder.
