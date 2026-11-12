# Một access unit video lỗi làm chết cả subprocess

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/transcoder/native, internal/transcoder
- **Vị trí:** internal/transcoder/native/decoder.go:222-225, internal/transcoder/native/stream_pipeline.go:850-853, internal/transcoder/native/stream_pipeline.go:889-892, internal/transcoder/native/server.go:92-95, internal/transcoder/native/audio_reencode.go:188-192

## Hiện tượng
Nguồn UDP/SRT có mất gói, sau một access unit hỏng thì mọi rendition cùng gián đoạn vài giây, `restart_count` tăng và có lỗi `pipeline: decode: ... Invalid data`. Encoder bị khởi tạo lại nên player có thể phải tái khởi tạo decoder.

## Điều kiện kích hoạt
Đường decode CPU (`h264`, `hevc`). Sau keyframe, `SendPacket` trả `AVERROR_INVALIDDATA` cho một packet, ví dụ packet không còn slice giải mã được do mất gói, hoặc packet chỉ chứa NAL không phải VCL.

## Phân tích
Mọi lỗi `SendPacket` đều được trả ngược lên:

```go
if err := d.codecCtx.SendPacket(d.pkt); err != nil {
    d.pkt.Unref()
    return nil, fmt.Errorf("native: SendPacket: %w", err)
}
```

`ProcessPacket` trả về lỗi, `Server.Run` gửi `terminalError` và thoát, supervisor respawn toàn bộ process.

## Vì sao đây là lỗi
Đường audio cố ý chịu lỗi: "A bad audio AU shouldn't kill the stream; skip it", còn video thì không. Cổng `sawKeyframe` chỉ bảo vệ giai đoạn trước IDR đầu tiên. Respawn làm mất SPS/PPS và trạng thái encoder, trái với mục tiêu output liên tục. Điều kiện chính xác để h264 decoder trả `INVALIDDATA` dựa trên kiến thức về libavcodec, chưa chạy thử.

## Hướng sửa
Với `AVERROR_INVALIDDATA` ở `SendPacket`, đếm lỗi, bỏ qua packet và chờ decoder phục hồi. Chỉ coi là terminal khi lỗi kéo dài (theo số lần hoặc thời gian liên tiếp). Đặt lại `sawKeyframe` nếu cần để tránh ra hình rác.

## Cách xác minh
Test: decode vài frame hợp lệ rồi đưa một packet chỉ có SEI hoặc byte ngẫu nhiên có start code qua `ProcessPacket`, kiểm tra lỗi trả về. Thực tế: tạo mất gói bằng `tc netem` trên input UDP của môi trường thử nghiệm riêng.
