# Reload từ passthrough sang transcoder bỏ rơi subscriber của buffer chính

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/buffer, internal/publisher, internal/ingestor/pull
- **Vị trí:** internal/coordinator/coordinator.go:663-689, internal/publisher/serve_rtmp.go:53-80, internal/publisher/serve_srt.go:121-146, internal/publisher/serve_mpegts.go:78-100, internal/ingestor/pull/copy.go:125-130, internal/coordinator/abr_mixer.go:466-480

## Hiện tượng
Bật transcoder trên stream passthrough đang có người xem: client RTMP, SRT, HTTP MPEG-TS vẫn kết nối nhưng không nhận thêm dữ liệu và không bị ngắt. Session vẫn hiện trong API. Stream ABR-mixer lấy audio từ upstream này mất audio vĩnh viễn cho tới khi restart stream mixer. Downstream `copy://` chỉ phục hồi sau khoảng packet timeout + probe (khoảng 40 giây).

## Điều kiện kích hoạt
`PUT` chuyển stream từ không transcoder sang có transcoder (`TranscoderTopologyChanged`), trong khi có subscriber trực tiếp trên `<code>`.

## Phân tích
`reloadTranscoderFull` tạo `$raw$<code>` và chuyển ingest sang đó, nhưng giữ nguyên buffer chính:
```go
newIngestID = buffer.RawIngestBufferID(new.Code)
if oldIngestID != newIngestID {
    c.buf.Create(newIngestID)
}
```
Không có Delete hay `UnsubscribeAll` trên `<code>`. Các play handler chỉ dừng khi ctx huỷ hoặc channel đóng (`buf.Delete`). `pub.Stop` không huỷ chúng vì ctx của chúng thuộc từng phiên kết nối. Sau reload, `<code>` không còn writer nên channel im lặng nhưng không đóng.

Với `abrMixerAudioForwardDirect`, không có timeout nên goroutine chờ vô hạn; `audioBufID` chốt lúc start.

## Vì sao đây là lỗi
Comment của `Buffer.Delete` nêu chính cơ chế đóng channel là tín hiệu để consumer thử lại. Đường rendition có Delete nên client thoát và kết nối lại đúng; đường buffer chính thiếu tín hiệu này.

## Hướng sửa
Gọi `buf.UnsubscribeAll(<code>)` trong reload khi ingest rời buffer chính (giữ buffer, đóng subscriber cũ) sau khi publisher đã dừng.

## Cách xác minh
Test coordinator với buffer thật: subscribe `<code>`, gọi `Update` thêm transcoder, khẳng định channel đóng. Trên môi trường thử: mở RTMP play, thêm transcoder và quan sát phiên treo.
