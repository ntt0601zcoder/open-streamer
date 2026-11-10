# Output AV1 và MPEG-2 bị gắn nhãn H.264 khi đưa vào buffer hub

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/transcoder, internal/domain
- **Vị trí:** internal/transcoder/native/stream_pipeline.go:1463-1470, internal/transcoder/native/stream_pipeline.go:1444-1454, internal/transcoder/native/server.go:222-233, internal/transcoder/supervisor.go:488-499, internal/domain/resolvers.go:30-36, internal/domain/transcoder.go:29-30

## Hiện tượng
Profile `codec: av1` hoặc `mp2v` trên đường CPU encode bình thường, nhưng HLS/DASH/RTMP đầu ra không phát được vì publisher coi dữ liệu là H.264.

## Điều kiện kích hoạt
`hw=none` (hoặc backend khác đường GPU) và profile `codec: av1` (`libsvtav1`) hoặc `mp2v` (`mpeg2video`).

## Phân tích
```go
func encoderCodecToES(name string) esFrameCodec {
    switch name {
    case "hevc", "libx265", "hevc_nvenc", ...:
        return esCodecH265
    default:
        return esCodecH264
    }
}
```

Comment ghi "libx264 / h264_* are the only encoders configurable today", nhưng `ResolveVideoEncoder` đã trả `libsvtav1` và `mpeg2video`. `esFrameCodec` và `proto.Codec` cũng không có giá trị cho AV1 hay MPEG-2, nên supervisor ghi `AVCodecH264` dù domain đã có `AVCodecAV1` và `AVCodecMPEG2Video`.

## Vì sao đây là lỗi
Domain khai báo hai codec này là output được hỗ trợ. Không có lỗi ở encoder nên vấn đề chỉ lộ ở phía player. Trên `hw=nvenc`, cùng cấu hình này rơi vào lỗi riêng ở `transcoder-04`.

## Hướng sửa
Thêm codec vào `esFrameCodec`, `proto.Codec` và hai bảng ánh xạ, kèm kiểm tra publisher hỗ trợ. Nếu chưa hỗ trợ, từ chối ở validate và cho `encoderCodecToES` báo lỗi thay vì mặc định H.264.

## Cách xác minh
Test bảng cho `encoderCodecToES("libsvtav1")`, và ffprobe output HLS của profile `av1`.
