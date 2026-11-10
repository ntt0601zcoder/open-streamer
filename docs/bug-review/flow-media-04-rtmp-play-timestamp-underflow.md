# RTMP play không xử lý ranh giới session, timestamp tràn số sau failover
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (serve_rtmp), ingestor/push (rtmp_writer), ingestor (worker)
- **Vị trí:** internal/publisher/serve_rtmp.go:147-155, 256-264, 280-298, 301-313; internal/ingestor/push/rtmp_writer.go:113-116, 213-246; internal/ingestor/worker.go:88, 96; internal/ingestor/push/rtmp_server.go:452; internal/publisher/push_rtmp.go:306-309

## Hiện tượng
Client đang xem qua RTMP bị đứng hình vĩnh viễn hoặc mất đồng bộ ngay sau khi input failover hoặc encoder đẩy lại. Client phải kết nối lại. Nếu nguồn mới khác SPS, client tiếp tục giải mã bằng cấu hình cũ.

## Điều kiện kích hoạt
1. Stream không transcode. Client RTMP đang play với `baseDTS` đã chốt.
2. Failover sang worker mới hoặc publisher mới, nên PTS về gần 0.

## Phân tích
Vòng feed chỉ reset muxer:
```go
if pkt.SessionStart {
    avMux = nil
    tsCarry = nil
}
```
`rtmpPlaySession` không biết có ranh giới, và tính timestamp bằng uint64:
```go
relDTS := uint32(dts - ps.baseDTS)
relPTS := uint32(pts - ps.baseDTS)
```
Khi `dts < baseDTS`, phép trừ tràn và kết quả gần `2^32`. Audio có cùng dạng với `baseADTS`. Client thấy timestamp nhảy gần 49 ngày.

Việc sequence header không được gửi lại đã có ở ingest-15 và publisher-30. File này chỉ nói về phép trừ tràn số trên timestamp, một lỗi riêng: kể cả khi SPS không đổi, client vẫn bị đứng.

## Vì sao đây là lỗi
Mọi output khác đều xử lý boundary: push RTMP đóng session và mở lại, RTSP gọi `onSessionBoundary` và rebase khi DTS lùi, HLS/DASH reset. Comment ở serve_rtmp chỉ xử lý phần muxer. Normaliser chỉ giữ `wallOrigin` trong một worker, không qua failover.

## Hướng sửa
Khi có `SessionStart`, truyền tín hiệu sang goroutine demux để rebase sao cho timestamp RTMP nối tiếp giá trị cuối, đặt lại `firstVideo` và cho phép gửi lại seq header. Cách đơn giản hơn là đóng play session để client kết nối lại.

## Cách xác minh
Unit test `rtmpPlaySession`: gọi `onTSFrame` với DTS 1.000.000 rồi DTS 40, kiểm tra timestamp không tràn. Thực tế: ffplay xem `rtmp://.../live/x` rồi ngắt encoder và đẩy lại.
