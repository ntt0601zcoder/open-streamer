# HTTP MPEG-TS trả 200 nhưng không có byte nào với stream AV hoặc stream transcode
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HTTP-TS), transcoder, ingestor
- **Vị trí:** internal/publisher/serve_mpegts.go:180-189; internal/transcoder/supervisor.go:438; internal/ingestor/worker.go:493; internal/ingestor/push/rtmp_server.go:598

## Hiện tượng
`GET /<code>/mpegts` trả `200 OK`, giữ kết nối mãi nhưng thân response rỗng. ffmpeg/VLC báo timeout hoặc không nhận được dữ liệu. Session vẫn hiện trong API với 0 byte.

## Điều kiện kích hoạt
Bật `protocols.mpegts` cho stream mà buffer phát (`mediaBuf`) chứa `Packet.AV`:
1. Mọi stream có transcoder (rendition được ghi bằng `buffer.Packet{AV: av}`).
2. Stream không transcode từ RTMP pull, RTSP pull, RTMP push, hoặc mixer.

## Phân tích
```go
data := pkt.TS
if len(data) == 0 && pkt.AV != nil {
    // ... transcoder output is also Packet.TS ...
    continue
}
```
Comment nói output transcoder là `Packet.TS`, nhưng `supervisor.go:438` tạo `buffer.Packet{AV: av, ...}`. `worker.go:493` và `rtmp_server.go:598` cũng ghi AV. Handler bỏ qua toàn bộ các packet này. SRT play cùng trường hợp lại dùng `tsmux.FeedWirePacket` để mux AV sang TS (`serve_srt.go:215`).

## Vì sao đây là lỗi
Không có guard nào từ chối request khi buffer là AV: `mpegtsTargetFor` chỉ kiểm tra stream chạy và cờ bật. Test `TestHandleMPEGTS_*` chỉ ghi `Packet.TS` nên không bắt được.

## Hướng sửa
Dùng `tsmux.FeedWirePacket` với một `*tsmux.FromAV` theo từng request, reset muxer và carry khi `pkt.SessionStart`, như SRT play.

## Cách xác minh
Unit test: ghi `buffer.Packet{AV: ...}` H.264 vào buffer rồi gọi handler, kiểm tra body có packet 188 byte. Thực tế: tạo stream có transcoder, `curl <host>/<code>/mpegts | wc -c`.
