# protocols.rtmp và protocols.srt không có tác dụng, mọi stream luôn phát được qua RTMP và SRT

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/domain, internal/publisher, internal/ingestor/push, internal/runtime
- **Vị trí:** internal/domain/protocols.go:20-24, internal/publisher/service.go:307-332, internal/publisher/service.go:447-503, internal/publisher/serve_rtmp.go:53-67, internal/publisher/serve_srt.go:96, internal/publisher/serve_srt.go:115-129, docs/CONFIG.md:313-318

## Hiện tượng
Stream cấu hình `protocols: {hls: true, rtmp: false, srt: false}` vẫn xem được bằng `rtmp://host/live/<code>` và SRT `streamid=live/<code>` khi listener bật. Ngược lại đặt `true` cũng không thay đổi gì. Operator tưởng đã tắt các đường phát này.

## Điều kiện kích hoạt
1. `listeners.rtmp.enabled: true`, stream đang chạy với `protocols.rtmp: false`.
2. `ffplay rtmp://host:1935/live/<code>`.

## Phân tích
Cả codebase không đọc `Protocols.RTMP` hay `Protocols.SRT`. `spawnOutputsLocked` và `UpdateProtocols` chỉ xử lý HLS, DASH, RTSP và push. Handler phát chỉ hỏi stream có đang chạy không:

```go
bufID, ok := s.mediaBufferFor(code)
if !ok {
    return fmt.Errorf("stream %q not active", key)
}
```

`mediaBuffer` có cho mọi stream đang chạy. `ss.mpegtsEnabled` là tiền lệ đúng: endpoint MPEG-TS có kiểm tra cờ tương ứng, còn RTMP và SRT thì không.

Diff vẫn coi việc đổi hai cờ này là `ProtocolsChanged`, nhưng không có gì được áp dụng.

## Vì sao đây là lỗi
docs/CONFIG.md ghi "Each true → publisher starts that output" và ví dụ có `rtmp: false`, `srt: false`. Media-auth vẫn áp dụng nếu stream có policy, nhưng stream không policy sẽ lộ công khai trên giao thức mà operator đã tắt.

## Hướng sửa
Mirror hai cờ vào `streamState` như `mpegtsEnabled` và kiểm tra trong `HandleRTMPPlay` và `srtHandleConnect`. Lưu ý tương thích với dữ liệu cũ đang để `false` mà vẫn dựa vào đường phát này.

## Cách xác minh
Test `HandleRTMPPlay` với stream `RTMP=false`, mong đợi lỗi. Thử thực tế bằng ffplay trên stream thử.
