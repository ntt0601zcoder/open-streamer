# Transcoder respawn đưa PTS rendition về gần 0 mà không có SessionStart
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** transcoder (supervisor, native), buffer, publisher (HLS, DASH), dvr
- **Vị trí:** internal/transcoder/supervisor.go:106-143, 431-438; internal/transcoder/native/stream_pipeline.go:380-393, 642-653, 1380-1385; internal/publisher/hls.go:311-316, 559, 681-683; internal/publisher/dash/frame_queue.go:149-152, 178-192, 349-359; internal/publisher/dash/segmenter.go:170, 212

## Hiện tượng
Sau khi subprocess transcoder restart (`restart_count` tăng), HLS của mọi rendition không có `EXT-X-DISCONTINUITY` dù PTS tụt về gần 0. Player đứng hoặc tua loạn. DASH ngừng ra segment trong khoảng 2-3 phút rồi tự chạy lại.

## Điều kiện kích hoạt
Subprocess kết thúc rồi được respawn, ví dụ terminal error `pipeline: decode: ...` từ libav, hoặc lỗi gRPC như transcoder-01. Stream vẫn tiếp tục. Đường crash không có terminal error còn bị chặn bởi flow-lifecycle-01; sửa lỗi đó thì đường này cũng trúng bug này.

## Phân tích
Mỗi lần `runOnce` tạo một `StreamPipeline` mới với `pendingRebase: true` và `lastVideoOut = 0`, nên output bắt đầu lại ở 1 ms:
```go
base := p.lastVideoOut
...
p.ptsOffset = base + 1 - srcPTS
```
`pendingSessionStart` chỉ được đặt trong `SwitchInput`. Supervisor cũng không đánh dấu gì:
```go
bufPkt := buffer.Packet{AV: av, SessionStart: pkt.GetSessionStart()}
```
Rendition buffer không được `SetSession` ở pipeline thường (grep `SetSession` chỉ có worker, watchdog, RTMP server, abr_copy, abr_mixer).

Phía tiêu thụ:
- HLS: không flush, không đặt `discNext`, và segment chứa cả PTS lớn lẫn PTS nhỏ.
- DASH: queue còn frame PTS lớn ở đầu, frame mới PTS nhỏ ở cuối. `VideoSpanMS` trả 0 vì `last <= first`, `trimVideoToSpan` dừng vì `newPTSms < first`, nên không cut được cho tới khi chạm `videoFrameHardCap` (3600 frame, 144 giây ở 25 fps).
- DVR: wallclock bị kéo lùi như flow-media-01.

## Vì sao đây là lỗi
Comment ở `SwitchInput` nói rõ timeline liên tục là điều kiện để player chạy được, nhưng respawn phá điều đó mà không báo. Toàn bộ consumer chỉ dựa vào `SessionStart` để biết ranh giới.

## Hướng sửa
Supervisor giữ PTS output lớn nhất và truyền qua `Configure` để pipeline mới nối tiếp. Đồng thời gắn `SessionStart=true` lên packet đầu sau respawn cho từng rendition.

## Cách xác minh
Giả lập respawn trong test supervisor: gọi `writeOutputPacket` với PTS lớn, chạy lại `runOnce` với fake server phát PTS 1, kiểm tra subscriber nhận `SessionStart`. Với DASH: push frame PTS lớn rồi nhỏ vào `FrameQueue`, kiểm tra `Cut` không kẹt.
